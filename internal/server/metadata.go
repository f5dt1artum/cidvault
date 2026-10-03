package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

// Metadata value bounds. contentType is null or a short printable string;
// labels are a small string map with identifier-like keys. Lengths count
// Unicode code points.
const (
	maxMetadataLabels         = 64
	maxMetadataContentTypeLen = 255
	maxMetadataLabelValueLen  = 256
)

// metadataLabelKey matches the only legal label key shape.
var metadataLabelKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// metadataRequest is the strictly decoded PUT body. RawMessage fields stay
// nil for missing keys but hold the literal "null" for JSON nulls, so the
// two cases stay distinguishable.
type metadataRequest struct {
	ExpectedRevision json.RawMessage    `json:"expectedRevision"`
	ContentType      json.RawMessage    `json:"contentType"`
	Labels           *map[string]string `json:"labels"`
}

// metadataResponse is the JSON body of metadata reads and writes. Labels
// encode as an object with keys in lexical order; contentType is null when
// unset.
type metadataResponse struct {
	CID         string            `json:"cid"`
	Revision    int64             `json:"revision"`
	ContentType *string           `json:"contentType"`
	Labels      map[string]string `json:"labels"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// metadataByID handles GET and PUT on /v1/objects/{cid}/metadata.
func (s *store) metadataByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getMetadata(w, r)
	case http.MethodPut:
		s.putMetadata(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// putMetadata handles PUT /v1/objects/{cid}/metadata.
func (s *store) putMetadata(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !wellFormedJSON(body) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var req metadataRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.ExpectedRevision == nil || req.ContentType == nil || req.Labels == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	expected, err := parseInt(req.ExpectedRevision)
	if err != nil || expected < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var contentType *string
	if err := json.Unmarshal(req.ContentType, &contentType); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if contentType != nil && !validMetadataContentType(*contentType) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_metadata")
		return
	}
	if !validMetadataLabels(*req.Labels) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_metadata")
		return
	}
	rev, created, code := s.commitMetadata(cid, int64(expected), contentType, *req.Labels, time.Now())
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
	writeMetadata(w, status, cid, rev)
}

// getMetadata handles GET /v1/objects/{cid}/metadata. Without query
// parameters it returns the current revision; a single positive decimal
// revision parameter selects a historical revision.
func (s *store) getMetadata(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var revision int64
	for key, values := range query {
		if key != "revision" || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		v, ok := parseAuditInt(values[0])
		if !ok || v < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		revision = v
	}
	rev, code := s.metadataAt(cid, revision)
	if code != "" {
		writeError(w, http.StatusNotFound, code)
		return
	}
	writeMetadata(w, http.StatusOK, cid, rev)
}

// writeMetadata encodes one revision; labels always encode as an object,
// never null.
func writeMetadata(w http.ResponseWriter, status int, cid string, rev metadataRevision) {
	labels := rev.labels
	if labels == nil {
		labels = map[string]string{}
	}
	writeJSON(w, status, metadataResponse{
		CID:         cid,
		Revision:    rev.revision,
		ContentType: rev.contentType,
		Labels:      labels,
		UpdatedAt:   rev.updatedAt,
	})
}

// validMetadataContentType reports whether s is 1 to 255 code points with no
// control characters.
func validMetadataContentType(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= maxMetadataContentTypeLen && !hasControlChar(s)
}

// validMetadataLabels reports whether the label set obeys the entry count,
// key shape and value bounds.
func validMetadataLabels(labels map[string]string) bool {
	if len(labels) > maxMetadataLabels {
		return false
	}
	for k, v := range labels {
		if !metadataLabelKey.MatchString(k) {
			return false
		}
		if utf8.RuneCountInString(v) > maxMetadataLabelValueLen || hasControlChar(v) {
			return false
		}
	}
	return true
}

// hasControlChar reports whether s contains a Unicode control character.
func hasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
