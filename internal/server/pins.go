package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"
)

// pinRequest is the JSON body accepted by PUT /v1/pins/{cid}. An absent or
// null expiresAt pins permanently.
type pinRequest struct {
	ExpiresAt *string `json:"expiresAt"`
}

// pinResponse is the JSON body returned by pin creation, update and listing.
// ExpiresAt is null for permanent pins.
type pinResponse struct {
	CID       string     `json:"cid"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// pinListResponse is the JSON body of GET /v1/pins.
type pinListResponse struct {
	Pins []pinResponse `json:"pins"`
}

// gcResponse is the JSON body of POST /v1/gc.
type gcResponse struct {
	DryRun  bool     `json:"dryRun"`
	Objects int      `json:"objects"`
	Bytes   int      `json:"bytes"`
	CIDs    []string `json:"cids"`
}

// pinByID handles PUT and DELETE on /v1/pins/{cid}.
func (s *store) pinByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.putPin(w, r)
	case http.MethodDelete:
		s.deletePin(w, r)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// putPin handles PUT /v1/pins/{cid}.
func (s *store) putPin(w http.ResponseWriter, r *http.Request) {
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
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var req pinRequest
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
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
	created, ok := s.setPin(cid, expiresAt, now)
	if !ok {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, pinResponse{CID: cid, ExpiresAt: expiresAt})
}

// deletePin handles DELETE /v1/pins/{cid}.
func (s *store) deletePin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	if !s.removePin(cid, time.Now()) {
		writeError(w, http.StatusNotFound, "pin_not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getPins handles GET /v1/pins.
func (s *store) getPins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	entries := s.listPins(time.Now())
	pins := make([]pinResponse, 0, len(entries))
	for _, e := range entries {
		pins = append(pins, pinResponse{CID: e.cid, ExpiresAt: e.expiresAt})
	}
	writeJSON(w, http.StatusOK, pinListResponse{Pins: pins})
}

// collectGarbage handles POST /v1/gc.
func (s *store) gc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	dryRun := false
	if values, present := r.URL.Query()["dryRun"]; present {
		if len(values) != 1 || values[0] != "true" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		dryRun = true
	}
	cids, totalBytes := s.collectGarbage(time.Now(), dryRun)
	writeJSON(w, http.StatusOK, gcResponse{
		DryRun:  dryRun,
		Objects: len(cids),
		Bytes:   totalBytes,
		CIDs:    cids,
	})
}
