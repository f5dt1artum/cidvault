package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"
)

// pinResponse is the JSON view of one pin, used by PUT and GET.
type pinResponse struct {
	CID       string     `json:"cid"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type pinListResponse struct {
	Pins []pinResponse `json:"pins"`
}

type gcResponse struct {
	DryRun  bool     `json:"dryRun"`
	Objects int      `json:"objects"`
	Bytes   int      `json:"bytes"`
	CIDs    []string `json:"cids"`
}

// maxPinBodySize bounds the JSON document accepted on PUT /v1/pins/{cid}.
const maxPinBodySize = 1 << 20

// pinByCID dispatches /v1/pins/{cid} between PUT and DELETE and answers any
// other method with one shared 405 (Allow: DELETE, PUT).
func (s *store) pinByCID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.putPin(w, r)
	case http.MethodDelete:
		s.deletePinHandler(w, r)
	default:
		w.Header().Set("Allow", "DELETE, PUT")
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
	expiresAt, errCode := decodePinExpiration(r.Body, now)
	if errCode != "" {
		writeError(w, statusForPinError(errCode), errCode)
		return
	}

	updated, ok := s.upsertPin(cid, expiresAt, now)
	if !ok {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	status := http.StatusCreated
	if updated {
		status = http.StatusOK
	}
	writeJSON(w, status, pinResponse{CID: cid, ExpiresAt: expiresAt})
}

func statusForPinError(code string) int {
	if code == "invalid_expiration" {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadRequest
}

// decodePinExpiration parses the PUT body and returns the parsed expiration.
// A nil result with an empty code means permanent retention. The returned
// code is "invalid_request" for malformed JSON or unknown fields and
// "invalid_expiration" for a missing/invalid timestamp or one not in the
// future.
func decodePinExpiration(body io.Reader, now time.Time) (*time.Time, string) {
	dec := json.NewDecoder(io.LimitReader(body, maxPinBodySize))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return nil, "invalid_request"
	}
	// A bare "null" decodes into a nil map without error but is not the
	// required JSON object.
	if fields == nil {
		return nil, "invalid_request"
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, "invalid_request"
	}
	for key := range fields {
		if key != "expiresAt" {
			return nil, "invalid_request"
		}
	}
	raw, present := fields["expiresAt"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, "invalid_expiration"
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, "invalid_expiration"
	}
	if !t.After(now) {
		return nil, "invalid_expiration"
	}
	return &t, ""
}

// listPinsHandler handles GET /v1/pins.
func (s *store) listPinsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	infos := s.listPins(time.Now())
	resp := pinListResponse{Pins: []pinResponse{}}
	for _, info := range infos {
		resp.Pins = append(resp.Pins, pinResponse{CID: info.cid, ExpiresAt: info.expiresAt})
	}
	writeJSON(w, http.StatusOK, resp)
}

// deletePinHandler handles DELETE /v1/pins/{cid}.
func (s *store) deletePinHandler(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	if !s.deletePin(cid, time.Now()) {
		writeError(w, http.StatusNotFound, "pin_not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gcHandler handles POST /v1/gc.
func (s *store) gcHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Only the literal dryRun=true selects preview mode; the parameter must
	// be omitted entirely for an actual collection.
	var dryRun bool
	switch v := r.URL.Query().Get("dryRun"); v {
	case "":
	case "true":
		dryRun = true
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	res := s.collectGarbage(time.Now(), dryRun)
	cids := res.cids
	if cids == nil {
		cids = []string{}
	}
	writeJSON(w, http.StatusOK, gcResponse{
		DryRun:  dryRun,
		Objects: len(res.cids),
		Bytes:   res.bytes,
		CIDs:    cids,
	})
}
