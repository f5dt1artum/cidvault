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

// proofDomain separates the sampling digests of this endpoint from every
// other SHA-256 use in the system.
const proofDomain = "cidvault-retrievability-v1"

// maxProofRequestSize bounds the wire size of a proof request. The body is
// a nonce of at most 64 decoded bytes plus a small integer; the slack
// covers JSON framing and whitespace.
const maxProofRequestSize = 1 << 12

// proofRequest is the strictly decoded JSON body of
// POST /v1/retrievability-proofs/{cid}. Pointer fields distinguish a
// missing key from a zero value; samples stays raw so quoted strings and
// fractional values are rejected as type errors.
type proofRequest struct {
	Nonce   *string          `json:"nonce"`
	Samples *json.RawMessage `json:"samples"`
}

// proofEntry is one sampled manifest position in the proof response.
type proofEntry struct {
	Index int    `json:"index"`
	CID   string `json:"cid"`
	Data  string `json:"data"`
}

// proofResponse is the JSON body returned by
// POST /v1/retrievability-proofs/{cid}.
type proofResponse struct {
	CID              string       `json:"cid"`
	Nonce            string       `json:"nonce"`
	RequestedSamples int          `json:"requestedSamples"`
	Entries          []proofEntry `json:"entries"`
}

// postRetrievabilityProof handles POST /v1/retrievability-proofs/{cid}: it
// deterministically samples manifest positions and returns the raw bytes of
// the selected chunks so a caller can verify the object is held locally.
// The endpoint is read-only: it creates no pins, appends no audit events
// and changes no store, provider or statistics state.
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProofRequestSize+1))
	if err != nil || len(body) > maxProofRequestSize {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	nonce, samples, code, status := parseProofRequest(body)
	if code != "" {
		writeError(w, status, code)
		return
	}
	// The object is immutable once stored, so resolving it under a single
	// read lock yields a consistent snapshot of the manifest and every
	// chunk: a concurrent sweep either still sees the object (full success)
	// or has already removed it (object_not_found), never a partial read.
	obj := s.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	writeJSON(w, http.StatusOK, proofResponse{
		CID:              obj.cid,
		Nonce:            nonce,
		RequestedSamples: samples,
		Entries:          selectProofEntries(obj, nonce, samples),
	})
}

// parseProofRequest strictly validates the request body. On failure it
// returns the error code and HTTP status to report; the store is never
// touched on the failure path.
func parseProofRequest(body []byte) (nonce string, samples int, code string, status int) {
	invalid := func() (string, int, string, int) {
		return "", 0, "invalid_request", http.StatusBadRequest
	}
	if !wellFormedJSON(body) {
		return invalid()
	}
	var req proofRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Nonce == nil || req.Samples == nil {
		return invalid()
	}

	// The nonce must be canonical RFC 4648 standard Base64: decoding and
	// re-encoding must reproduce the input exactly, which also rejects
	// non-zero trailing padding bits.
	raw, err := base64.StdEncoding.DecodeString(*req.Nonce)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != *req.Nonce ||
		len(raw) < 16 || len(raw) > 64 {
		return "", 0, "invalid_nonce", http.StatusUnprocessableEntity
	}

	// Samples must be a JSON integer; an out-of-range integer is a value
	// problem, not a type problem.
	n, err := parseInt(*req.Samples)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return "", 0, "invalid_sample_count", http.StatusUnprocessableEntity
		}
		return invalid()
	}
	if n < 1 || n > 16 {
		return "", 0, "invalid_sample_count", http.StatusUnprocessableEntity
	}
	return *req.Nonce, n, "", 0
}

// selectProofEntries deterministically samples manifest positions. Every
// position is scored by the SHA-256 of the domain separator, root cid,
// canonical nonce and decimal index, each followed by a newline; positions
// are ranked by digest in unsigned lexicographic order with ties broken by
// ascending index, and the first min(samples, positions) win. Repeated
// chunks participate once per position. The returned entries are ordered
// by ascending manifest index.
func selectProofEntries(obj *object, nonce string, samples int) []proofEntry {
	type scored struct {
		index  int
		digest [sha256.Size]byte
	}
	positions := make([]scored, len(obj.cids))
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
		var digest [sha256.Size]byte
		copy(digest[:], h.Sum(nil))
		positions[i] = scored{index: i, digest: digest}
	}
	sort.Slice(positions, func(a, b int) bool {
		if c := bytes.Compare(positions[a].digest[:], positions[b].digest[:]); c != 0 {
			return c < 0
		}
		return positions[a].index < positions[b].index
	})
	take := samples
	if take > len(positions) {
		take = len(positions)
	}
	selected := positions[:take]
	sort.Slice(selected, func(a, b int) bool { return selected[a].index < selected[b].index })
	entries := make([]proofEntry, 0, take)
	for _, p := range selected {
		entries = append(entries, proofEntry{
			Index: p.index,
			CID:   obj.cids[p.index],
			Data:  base64.StdEncoding.EncodeToString(obj.chunks[p.index]),
		})
	}
	return entries
}
