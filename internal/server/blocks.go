package server

import (
	"io"
	"net/http"
)

// statsResponse is the JSON body of GET /v1/storage/stats. Objects counts
// every live object (empty ones included); logicalBytes sums their body
// sizes. Blocks counts the unique blocks referenced by live objects and
// storedBytes sums their raw lengths, each shared block counted once.
type statsResponse struct {
	Objects      int `json:"objects"`
	LogicalBytes int `json:"logicalBytes"`
	Blocks       int `json:"blocks"`
	StoredBytes  int `json:"storedBytes"`
}

// getBlockByID handles GET and HEAD /v1/blocks/{cid}. Both share the
// conditional and range-aware read path; HEAD carries the full response
// headers, ignores Range and never writes a body.
func (s *store) getBlockByID(w http.ResponseWriter, r *http.Request) {
	if !readMethodAllowed(w, r) {
		return
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeReadError(w, r, http.StatusBadRequest, "invalid_cid")
		return
	}
	data := s.getBlock(cid)
	if data == nil {
		writeReadError(w, r, http.StatusNotFound, "block_not_found")
		return
	}
	serveStoredContent(w, r, cid, "application/octet-stream", len(data),
		func(w io.Writer, start, end int) { _, _ = w.Write(data[start : end+1]) })
}

// getStorageStats handles GET /v1/storage/stats.
func (s *store) getStorageStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	st := s.stats()
	writeJSON(w, http.StatusOK, statsResponse{
		Objects:      st.objects,
		LogicalBytes: st.logicalBytes,
		Blocks:       st.blocks,
		StoredBytes:  st.storedBytes,
	})
}
