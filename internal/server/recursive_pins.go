package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"
)

// maxRecursivePinDepth bounds the depth of a recursively pinned tree. The
// root is depth 0; a member deeper than this rejects the request.
const maxRecursivePinDepth = 64

// maxRecursivePinMembers bounds the number of unique members (root
// included) one recursive pin record may protect.
const maxRecursivePinMembers = 10000

// recursivePinResponse is the JSON body returned by recursive pin creation,
// update and reads. ExpiresAt is null for permanent records; members is the
// commit-time snapshot of unique member cids, sorted lexically.
type recursivePinResponse struct {
	CID       string     `json:"cid"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Objects   int        `json:"objects"`
	Bytes     int        `json:"bytes"`
	Members   []string   `json:"members"`
}

// recursivePinResponseOf renders a stored record for the wire.
func recursivePinResponseOf(cid string, rp recursivePin) recursivePinResponse {
	return recursivePinResponse{
		CID:       cid,
		ExpiresAt: rp.expiresAt,
		Objects:   len(rp.members),
		Bytes:     rp.bytes,
		Members:   rp.members,
	}
}

// recursivePinByID handles GET, PUT and DELETE on /v1/recursive-pins/{cid}.
func (s *store) recursivePinByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getRecursivePin(w, r)
	case http.MethodPut:
		s.putRecursivePin(w, r)
	case http.MethodDelete:
		s.deleteRecursivePin(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// putRecursivePin handles PUT /v1/recursive-pins/{cid}.
func (s *store) putRecursivePin(w http.ResponseWriter, r *http.Request) {
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
	now := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil || !wellFormedJSON(body) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var req pinRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil || !t.After(now) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_expiration")
			return
		}
		expiresAt = &t
	}
	rp, created, code := s.setRecursivePin(cid, expiresAt, now)
	if code != "" {
		writeError(w, recursivePinErrorStatus(code), code)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, recursivePinResponseOf(cid, rp))
}

// getRecursivePin handles GET /v1/recursive-pins/{cid}: the record in force
// together with its commit-time member snapshot.
func (s *store) getRecursivePin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	rp, ok := s.recursivePinAt(cid, time.Now())
	if !ok {
		writeError(w, http.StatusNotFound, "recursive_pin_not_found")
		return
	}
	writeJSON(w, http.StatusOK, recursivePinResponseOf(cid, rp))
}

// deleteRecursivePin handles DELETE /v1/recursive-pins/{cid}. Deletion only
// drops the retention record; the member objects stay until a later sweep.
func (s *store) deleteRecursivePin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	if !s.removeRecursivePin(cid, time.Now()) {
		writeError(w, http.StatusNotFound, "recursive_pin_not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recursivePinErrorStatus maps a traversal error code to its HTTP status.
func recursivePinErrorStatus(code string) int {
	switch code {
	case "object_not_found":
		return http.StatusNotFound
	case "recursive_pin_target_missing":
		return http.StatusFailedDependency
	default: // invalid_directory, recursive_pin_too_large
		return http.StatusUnprocessableEntity
	}
}
