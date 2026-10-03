package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
)

// deltaBundleMediaType is the media type produced by the delta export and
// required of delta imports.
const deltaBundleMediaType = "application/vnd.cidvault.delta-bundle+json"

// deltaBundleErrors are the error codes of the incremental delta bundle.
var deltaBundleErrors = bundleErrors{
	invalid:     "invalid_delta_bundle",
	unsupported: "unsupported_delta_bundle",
	missing:     "missing_block",
}

// haveRequest is the strictly decoded body of POST /v1/delta-bundles/{cid}.
// The pointer distinguishes a missing or null "have" from an empty list.
type haveRequest struct {
	Have *[]string `json:"have"`
}

// exportDeltaBundle handles POST /v1/delta-bundles/{cid}: it answers the
// object's manifest plus exactly the unique blocks the requester did not
// list as already held.
func (s *store) exportDeltaBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	obj := s.lookupObject(w, r)
	if obj == nil {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	have, ok := parseHave(body)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	dataByCID := make(map[string][]byte, len(obj.cids))
	for i, c := range obj.cids {
		dataByCID[c] = obj.chunks[i]
	}
	cids := make([]string, 0, len(dataByCID))
	for c := range dataByCID {
		if !have[c] {
			cids = append(cids, c)
		}
	}
	sort.Strings(cids)
	blocks := make([]bundleBlock, 0, len(cids))
	for _, c := range cids {
		blocks = append(blocks, bundleBlock{
			CID:  c,
			Data: base64.StdEncoding.EncodeToString(dataByCID[c]),
		})
	}
	w.Header().Set("Content-Type", deltaBundleMediaType)
	_ = json.NewEncoder(w).Encode(bundleDocument{
		Version: bundleVersion,
		Root: bundleRoot{
			CID:       obj.cid,
			Size:      obj.size,
			ChunkSize: ChunkSize,
			Chunks:    obj.cids,
		},
		Blocks: blocks,
	})
}

// parseHave strictly decodes a delta export request body: a single JSON
// object carrying only "have", a list of distinct well-formed block
// identifiers. Entries the manifest does not reference are ignored by the
// caller, not rejected here.
func parseHave(body []byte) (map[string]bool, bool) {
	if !wellFormedJSON(body) {
		return nil, false
	}
	var req haveRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, false
	}
	if req.Have == nil {
		return nil, false
	}
	have := make(map[string]bool, len(*req.Have))
	for _, c := range *req.Have {
		if !validCID(c) || have[c] {
			return nil, false
		}
		have[c] = true
	}
	return have, true
}

// importDeltaBundle handles POST /v1/delta-bundles: it rebuilds the object
// from the bundle's blocks plus the blocks the local store still holds,
// validates the result exactly like a full bundle, and stores it atomically.
func (s *store) importDeltaBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != deltaBundleMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBundleSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_delta_bundle")
		return
	}
	if len(body) > maxBundleSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	obj, code, status := parseBundleBody(body, deltaBundleErrors, s.getBlock)
	if code != "" {
		writeError(w, status, code)
		return
	}
	stored, created := s.put(obj)
	respStatus := http.StatusOK
	if created {
		respStatus = http.StatusCreated
	}
	writeJSON(w, respStatus, objectResponse{
		CID:       stored.cid,
		Size:      stored.size,
		ChunkSize: ChunkSize,
		Chunks:    stored.cids,
		Created:   created,
	})
}
