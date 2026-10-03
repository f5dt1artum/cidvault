package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
)

// deltaBundleMediaType is the media type exchanged by the incremental
// bundle endpoints. The document shape is identical to a full bundle; only
// the block set and the reconstruction rules differ.
const deltaBundleMediaType = "application/vnd.cidvault.delta-bundle+json"

// haveRequest is the JSON body of POST /v1/delta-bundles/{cid}: the block
// identifiers the receiver already holds.
type haveRequest struct {
	Have *[]string `json:"have"`
}

// postDeltaBundleByID handles POST /v1/delta-bundles/{cid}: export only the
// manifest-referenced blocks the receiver does not list in "have".
func (s *store) postDeltaBundleByID(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBundleSize+1))
	if err != nil || len(body) > maxBundleSize {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	have, ok := parseHaveRequest(body)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	obj := s.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}

	// Map each unique referenced identifier to its raw bytes, then drop
	// every identifier the receiver already holds.
	dataByCID := make(map[string][]byte, len(obj.cids))
	for i, c := range obj.cids {
		dataByCID[c] = obj.chunks[i]
	}
	cids := make([]string, 0, len(dataByCID))
	for c := range dataByCID {
		if have[c] {
			continue
		}
		cids = append(cids, c)
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

// parseHaveRequest strictly decodes {"have":[...]}. Every entry must be a
// well-formed identifier and no identifier may repeat; entries unrelated to
// the exported object are tolerated by the caller, which simply skips them.
func parseHaveRequest(body []byte) (map[string]bool, bool) {
	if !wellFormedJSON(body) {
		return nil, false
	}
	var req haveRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Have == nil {
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

// postDeltaBundle handles POST /v1/delta-bundles: reconstruct an object from
// the bundled blocks combined with blocks the local store still references.
func (s *store) postDeltaBundle(w http.ResponseWriter, r *http.Request) {
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
	content, code, status := parseDeltaBundle(body)
	if code != "" {
		writeError(w, status, code)
		return
	}
	obj, code, status := s.assembleDelta(content)
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

// deltaContent is the validated result of parsing an incremental bundle:
// the manifest claims and the raw bytes of every block carried inline. Blocks
// referenced but not carried must be supplied from the local store.
type deltaContent struct {
	cid    string
	size   int
	cids   []string
	blocks map[string][]byte
}

// parseDeltaBundle fully validates everything checkable without touching the
// store. On failure it returns the error code and HTTP status; the store is
// never touched on the failure path.
func parseDeltaBundle(body []byte) (*deltaContent, string, int) {
	invalid := func() (*deltaContent, string, int) {
		return nil, "invalid_delta_bundle", http.StatusBadRequest
	}
	integrity := func() (*deltaContent, string, int) {
		return nil, "integrity_check_failed", http.StatusUnprocessableEntity
	}

	if !wellFormedJSON(body) {
		return invalid()
	}
	var doc bundleJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return invalid()
	}
	if doc.Version == nil || doc.Root == nil || doc.Blocks == nil ||
		doc.Root.CID == nil || doc.Root.Size == nil || doc.Root.ChunkSize == nil ||
		doc.Root.Chunks == nil {
		return invalid()
	}
	for _, b := range *doc.Blocks {
		if b.CID == nil || b.Data == nil {
			return invalid()
		}
	}

	// Same integer parsing policy as full bundles: an out-of-range version
	// or chunk size is an unsupported value, an out-of-range size is too
	// large, and anything non-integral is a malformed document.
	version, err := parseInt(*doc.Version)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, "unsupported_delta_bundle", http.StatusUnprocessableEntity
		}
		return invalid()
	}
	chunkSize, err := parseInt(*doc.Root.ChunkSize)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, "unsupported_delta_bundle", http.StatusUnprocessableEntity
		}
		return invalid()
	}
	size, err := parseInt(*doc.Root.Size)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, "payload_too_large", http.StatusRequestEntityTooLarge
		}
		return invalid()
	}

	if !validCID(*doc.Root.CID) {
		return invalid()
	}
	for _, c := range *doc.Root.Chunks {
		if !validCID(c) {
			return invalid()
		}
	}
	blockData := make(map[string][]byte, len(*doc.Blocks))
	for _, b := range *doc.Blocks {
		if !validCID(*b.CID) {
			return invalid()
		}
		data, err := base64.StdEncoding.DecodeString(*b.Data)
		if err != nil {
			return invalid()
		}
		blockData[*b.CID] = data
	}

	if version != bundleVersion || chunkSize != ChunkSize {
		return nil, "unsupported_delta_bundle", http.StatusUnprocessableEntity
	}
	if size > MaxObjectSize {
		return nil, "payload_too_large", http.StatusRequestEntityTooLarge
	}
	if size < 0 {
		return integrity()
	}

	// Carried blocks must be a subset of the unique referenced identifiers:
	// no duplicates and nothing the manifest does not reference. Missing
	// blocks are expected in an incremental bundle and come from the store.
	referenced := make(map[string]bool, len(*doc.Root.Chunks))
	for _, c := range *doc.Root.Chunks {
		referenced[c] = true
	}
	seen := make(map[string]bool, len(*doc.Blocks))
	for _, b := range *doc.Blocks {
		if seen[*b.CID] || !referenced[*b.CID] {
			return integrity()
		}
		seen[*b.CID] = true
	}

	// Every carried block identifier must be the digest of its decoded bytes.
	for cid, data := range blockData {
		if chunkCID(data) != cid {
			return integrity()
		}
	}

	cids := *doc.Root.Chunks
	if (size == 0) != (len(cids) == 0) {
		return integrity()
	}
	return &deltaContent{
		cid:    *doc.Root.CID,
		size:   size,
		cids:   append([]string{}, cids...),
		blocks: blockData,
	}, "", 0
}

// assembleDelta merges the carried blocks with blocks still referenced by
// local objects, then applies the same fixed chunking, length and root
// checks as a full-bundle import. A referenced block held nowhere yields
// missing_block. The store itself is not mutated here; put happens only once
// the returned object is fully validated.
func (s *store) assembleDelta(content *deltaContent) (*object, string, int) {
	chunks := make([][]byte, len(content.cids))
	for i, c := range content.cids {
		if data, ok := content.blocks[c]; ok {
			chunks[i] = data
			continue
		}
		data := s.getBlock(c)
		if data == nil {
			return nil, "missing_block", http.StatusUnprocessableEntity
		}
		chunks[i] = data
	}

	total := 0
	for i, data := range chunks {
		n := len(data)
		if i < len(chunks)-1 {
			if n != ChunkSize {
				return nil, "integrity_check_failed", http.StatusUnprocessableEntity
			}
		} else if n < 1 || n > ChunkSize {
			return nil, "integrity_check_failed", http.StatusUnprocessableEntity
		}
		total += n
	}
	if total > MaxObjectSize {
		return nil, "payload_too_large", http.StatusRequestEntityTooLarge
	}
	if total != content.size {
		return nil, "integrity_check_failed", http.StatusUnprocessableEntity
	}
	if rootCID(content.size, content.cids) != content.cid {
		return nil, "integrity_check_failed", http.StatusUnprocessableEntity
	}
	return &object{
		cid:    content.cid,
		size:   content.size,
		chunks: chunks,
		cids:   content.cids,
	}, "", 0
}
