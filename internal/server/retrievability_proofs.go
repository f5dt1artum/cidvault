package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
)

// proofDomain prefixes the sampling transcript so proof digests cannot be
// confused with any other SHA-256 usage in the system.
const proofDomain = "cidvault-retrievability-v1"

// proofNonceMinBytes and proofNonceMaxBytes bound the decoded nonce length.
const (
	proofNonceMinBytes = 16
	proofNonceMaxBytes = 64
)

// proofMinSamples and proofMaxSamples bound the requested sample count.
const (
	proofMinSamples = 1
	proofMaxSamples = 16
)

// proofEntry is one sampled manifest position in the proof response. Data is
// the raw chunk bytes encoded with standard (padded) Base64.
type proofEntry struct {
	Index int    `json:"index"`
	CID   string `json:"cid"`
	Data  string `json:"data"`
}

// proofResponse is the JSON body of POST /v1/retrievability-proofs/{cid}.
// Nonce is the canonical form of the requested nonce; RequestedSamples echoes
// the request, while Entries holds min(samples, manifest positions) items
// ordered by manifest index.
type proofResponse struct {
	CID              string       `json:"cid"`
	Nonce            string       `json:"nonce"`
	RequestedSamples int          `json:"requestedSamples"`
	Entries          []proofEntry `json:"entries"`
}

// postRetrievabilityProof handles POST /v1/retrievability-proofs/{cid}. The
// endpoint is read-only: it creates no pins, appends no audit events and
// leaves every store counter untouched.
func (s *store) postRetrievabilityProof(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	nonce, samplesRaw, ok := parseProofRequest(body)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	nonceBytes, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil || base64.StdEncoding.EncodeToString(nonceBytes) != nonce ||
		len(nonceBytes) < proofNonceMinBytes || len(nonceBytes) > proofNonceMaxBytes {
		writeError(w, http.StatusUnprocessableEntity, "invalid_nonce")
		return
	}
	samples, err := strconv.ParseInt(samplesRaw, 10, 64)
	if err != nil {
		// A non-integer literal is a type error; an integer that overflows
		// int64 is simply out of range.
		if errors.Is(err, strconv.ErrRange) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_sample_count")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request")
		}
		return
	}
	if samples < proofMinSamples || samples > proofMaxSamples {
		writeError(w, http.StatusUnprocessableEntity, "invalid_sample_count")
		return
	}
	// The object is immutable once stored, so the manifest and chunk bytes
	// read here describe one consistent instant; a concurrent sweep either
	// still sees the object (full success) or has already removed it (404).
	obj := s.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	normalizedNonce := base64.StdEncoding.EncodeToString(nonceBytes)
	writeJSON(w, http.StatusOK, proofResponse{
		CID:              obj.cid,
		Nonce:            normalizedNonce,
		RequestedSamples: int(samples),
		Entries:          sampleManifest(obj, normalizedNonce, int(samples)),
	})
}

// parseProofRequest decodes the request body as exactly one JSON object with
// exactly the fields nonce (string) and samples (number). Missing, unknown,
// duplicated or mistyped fields and any trailing data are rejected.
func parseProofRequest(body []byte) (nonce string, samplesRaw string, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", "", false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return "", "", false
	}
	var nonceRaw, samplesJSON json.RawMessage
	seen := make(map[string]bool, 2)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", "", false
		}
		key, isString := tok.(string)
		if !isString || seen[key] {
			return "", "", false
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", "", false
		}
		switch key {
		case "nonce":
			nonceRaw = raw
		case "samples":
			samplesJSON = raw
		default:
			return "", "", false
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return "", "", false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return "", "", false
	}
	if nonceRaw == nil || samplesJSON == nil {
		return "", "", false
	}
	var noncePtr *string
	if err := json.Unmarshal(nonceRaw, &noncePtr); err != nil || noncePtr == nil {
		return "", "", false
	}
	// Samples must be a JSON number literal, not a string, boolean or null;
	// the decoder has already validated the literal itself.
	if c := samplesJSON[0]; c != '-' && (c < '0' || c > '9') {
		return "", "", false
	}
	var num json.Number
	if err := json.Unmarshal(samplesJSON, &num); err != nil {
		return "", "", false
	}
	return *noncePtr, num.String(), true
}

// sampleManifest selects up to samples manifest positions by the public
// challenge transcript and returns them ordered by index. Every position
// participates on its own, so a repeated chunk can be selected at several
// indices.
func sampleManifest(obj *object, nonce string, samples int) []proofEntry {
	type position struct {
		index  int
		digest [sha256.Size]byte
	}
	positions := make([]position, len(obj.cids))
	for i := range obj.cids {
		h := sha256.New()
		h.Write([]byte(proofDomain))
		h.Write([]byte{'\n'})
		h.Write([]byte(obj.cid))
		h.Write([]byte{'\n'})
		h.Write([]byte(nonce))
		h.Write([]byte{'\n'})
		h.Write([]byte(strconv.Itoa(i)))
		h.Write([]byte{'\n'})
		positions[i].index = i
		h.Sum(positions[i].digest[:0])
	}
	sort.Slice(positions, func(a, b int) bool {
		if c := bytes.Compare(positions[a].digest[:], positions[b].digest[:]); c != 0 {
			return c < 0
		}
		return positions[a].index < positions[b].index
	})
	if samples > len(positions) {
		samples = len(positions)
	}
	selected := positions[:samples]
	sort.Slice(selected, func(a, b int) bool { return selected[a].index < selected[b].index })
	entries := make([]proofEntry, 0, len(selected))
	for _, p := range selected {
		entries = append(entries, proofEntry{
			Index: p.index,
			CID:   obj.cids[p.index],
			Data:  base64.StdEncoding.EncodeToString(obj.chunks[p.index]),
		})
	}
	return entries
}
