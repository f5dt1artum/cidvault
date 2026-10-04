package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

// objectResponse is the JSON body returned by POST /v1/objects.
type objectResponse struct {
	CID       string   `json:"cid"`
	Size      int      `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
	Created   bool     `json:"created"`
}

// manifestResponse is the JSON body of GET /v1/objects/{cid}/manifest.
type manifestResponse struct {
	CID       string   `json:"cid"`
	Size      int      `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

// postObject handles POST /v1/objects.
func (s *store) postObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxObjectSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(body) > MaxObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}

	obj := newObject(body)
	stored, created := s.put(obj, auditObjectUpload, nil)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, objectResponse{
		CID:       stored.cid,
		Size:      stored.size,
		ChunkSize: ChunkSize,
		Chunks:    stored.cids,
		Created:   created,
	})
}

// newObject splits a body into chunks under the fixed rules and derives its
// root identifier from the resulting manifest.
func newObject(body []byte) *object {
	obj := &object{size: len(body), chunks: [][]byte{}, cids: []string{}}
	for start := 0; start < len(body); start += ChunkSize {
		end := start + ChunkSize
		if end > len(body) {
			end = len(body)
		}
		chunk := body[start:end]
		obj.chunks = append(obj.chunks, chunk)
		obj.cids = append(obj.cids, chunkCID(chunk))
	}
	obj.cid = rootCID(obj.size, obj.cids)
	return obj
}

// lookupObject validates the path identifier and resolves the object,
// writing the appropriate error response on failure.
func (s *store) lookupObject(w http.ResponseWriter, r *http.Request) *object {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return nil
	}
	obj := s.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return nil
	}
	return obj
}

// getObject handles GET and HEAD /v1/objects/{cid}.
func (s *store) getObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", allowGetHead)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Method == http.MethodHead {
		w = headWriter{w}
	}
	obj := s.lookupObject(w, r)
	if obj == nil {
		return
	}
	serveBytes(w, r, obj.body(), "application/octet-stream", strongETag(obj.cid))
}

// getManifest handles GET /v1/objects/{cid}/manifest.
func (s *store) getManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	obj := s.lookupObject(w, r)
	if obj == nil {
		return
	}
	writeJSON(w, http.StatusOK, manifestResponse{
		CID:       obj.cid,
		Size:      obj.size,
		ChunkSize: ChunkSize,
		Chunks:    obj.cids,
	})
}
