package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// refName matches the only legal reference name shape, applied to the
// percent-decoded path segment.
var refName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// refRequest is the strictly decoded PUT body. RawMessage fields stay nil
// for missing keys, so absent fields stay distinguishable from JSON nulls.
type refRequest struct {
	CID              json.RawMessage `json:"cid"`
	ExpectedRevision json.RawMessage `json:"expectedRevision"`
}

// refResponse is the JSON body of reference reads and writes.
type refResponse struct {
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	CID       string    `json:"cid"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// refByName handles GET and PUT on /v1/refs/{name}.
func (s *store) refByName(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getRef(w, r)
	case http.MethodPut:
		s.putRef(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// putRef handles PUT /v1/refs/{name}.
func (s *store) putRef(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !refName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_ref")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !wellFormedJSON(body) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var req refRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.CID == nil || req.ExpectedRevision == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var cid string
	if req.CID[0] != '"' || json.Unmarshal(req.CID, &cid) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected, err := parseInt(req.ExpectedRevision)
	if err != nil || expected < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	rev, created, code := s.commitRef(name, cid, int64(expected), time.Now())
	switch code {
	case "object_not_found":
		writeError(w, http.StatusNotFound, code)
		return
	case "revision_conflict":
		writeError(w, http.StatusConflict, code)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeRef(w, status, name, rev)
}

// getRef handles GET /v1/refs/{name}. Without query parameters it returns
// the current revision; a single positive decimal revision parameter
// selects a historical revision.
func (s *store) getRef(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !refName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_ref")
		return
	}
	revision, ok := refRevisionQuery(r.URL.RawQuery)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	rev, code := s.refAt(name, revision)
	if code != "" {
		writeError(w, http.StatusNotFound, code)
		return
	}
	writeRef(w, http.StatusOK, name, rev)
}

// refObjectByName handles GET on /v1/refs/{name}/object, returning the raw
// bytes of the object the selected revision points at.
func (s *store) refObjectByName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	name := r.PathValue("name")
	if !refName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_ref")
		return
	}
	revision, ok := refRevisionQuery(r.URL.RawQuery)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	rev, obj, code := s.refObjectAt(name, revision)
	switch code {
	case "ref_not_found", "ref_revision_not_found":
		writeError(w, http.StatusNotFound, code)
		return
	case "reference_target_missing":
		writeError(w, http.StatusFailedDependency, code)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(obj.size))
	w.Header().Set("X-CidVault-CID", rev.cid)
	w.Header().Set("X-CidVault-Ref-Revision", strconv.FormatInt(rev.revision, 10))
	for _, chunk := range obj.chunks {
		_, _ = w.Write(chunk)
	}
}

// writeRef encodes one reference revision.
func writeRef(w http.ResponseWriter, status int, name string, rev refRevision) {
	writeJSON(w, status, refResponse{
		Name:      name,
		Revision:  rev.revision,
		CID:       rev.cid,
		UpdatedAt: rev.updatedAt,
	})
}

// refRevisionQuery extracts the optional revision selector from a raw query.
// Only a single positive decimal revision parameter is legal.
func refRevisionQuery(rawQuery string) (int64, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return 0, false
	}
	var revision int64
	for key, values := range query {
		if key != "revision" || len(values) != 1 {
			return 0, false
		}
		v, ok := parseAuditInt(values[0])
		if !ok || v < 1 {
			return 0, false
		}
		revision = v
	}
	return revision, true
}
