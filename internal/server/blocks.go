package server

import (
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

// getBlockByID handles GET and HEAD /v1/blocks/{cid}.
func (s *store) getBlockByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", allowGetHead)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Method == http.MethodHead {
		w = headWriter{w}
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	data := s.getBlock(cid)
	if data == nil {
		writeError(w, http.StatusNotFound, "block_not_found")
		return
	}
	serveBytes(w, r, data, "application/octet-stream", strongETag(cid))
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
