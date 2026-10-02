package server

import (
	"net/http"
	"strconv"
)

// storageStatsResponse is the JSON body of GET /v1/storage/stats.
type storageStatsResponse struct {
	Objects      int `json:"objects"`
	LogicalBytes int `json:"logicalBytes"`
	Blocks       int `json:"blocks"`
	StoredBytes  int `json:"storedBytes"`
}

// getBlock handles GET /v1/blocks/{cid}. It returns the raw bytes of a
// chunk referenced by at least one current object.
func (s *store) getBlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	data := s.blockBytes(cid)
	if data == nil {
		writeError(w, http.StatusNotFound, "block_not_found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// getStorageStats handles GET /v1/storage/stats.
func (s *store) getStorageStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	st := s.stats()
	writeJSON(w, http.StatusOK, storageStatsResponse{
		Objects:      st.objects,
		LogicalBytes: st.logicalBytes,
		Blocks:       st.blocks,
		StoredBytes:  st.storedBytes,
	})
}
