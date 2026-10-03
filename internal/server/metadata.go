package server

import (
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxMetadataSize bounds the wire size of a metadata write body. The largest
// legal document is dominated by 64 labels of at most 256 code points each;
// the slack covers JSON framing, keys and escaping.
const maxMetadataSize = 1 << 20

// maxLabels bounds the number of label entries on one revision.
const maxLabels = 64

// metadataRevision is one immutable revision of an object's metadata.
// Revisions are never mutated once committed; a write always appends a new
// one, even when the content is identical to the previous revision.
type metadataRevision struct {
	revision    int64
	contentType *string
	labels      map[string]string
	updatedAt   time.Time
}

// metadataHistory is the full revision sequence of one object. Revisions
// are consecutive starting at 1, so revision N sits at index N-1.
type metadataHistory struct {
	revisions []metadataRevision
}

// metadataResponse is the JSON body returned by metadata reads and writes.
// Labels encode in key order because encoding/json sorts map keys.
type metadataResponse struct {
	CID         string            `json:"cid"`
	Revision    int64             `json:"revision"`
	ContentType *string           `json:"contentType"`
	Labels      map[string]string `json:"labels"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

// metadataByID handles PUT and GET on /v1/objects/{cid}/metadata.
func (s *store) metadataByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.putMetadata(w, r)
	case http.MethodGet:
		s.getMetadata(w, r)
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMetadataSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(body) > maxMetadataSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	expected, contentType, labels, code, status := parseMetadataPut(body)
	if code != "" {
		writeError(w, status, code)
		return
	}
	rev, created, code := s.commitMetadata(cid, expected, contentType, labels, time.Now().UTC())
	switch code {
	case "object_not_found":
		writeError(w, http.StatusNotFound, code)
		return
	case "revision_conflict":
		writeError(w, http.StatusConflict, code)
		return
	}
	respStatus := http.StatusOK
	if created {
		respStatus = http.StatusCreated
	}
	writeJSON(w, respStatus, metadataResponse{
		CID:         cid,
		Revision:    rev.revision,
		ContentType: rev.contentType,
		Labels:      rev.labels,
		UpdatedAt:   rev.updatedAt,
	})
}

// getMetadata handles GET /v1/objects/{cid}/metadata.
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
	revision := int64(0) // 0 selects the current revision
	for key, values := range query {
		if key != "revision" || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		v, ok := parsePositiveDecimal(values[0])
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		revision = v
	}
	rev, code := s.metadataAt(cid, revision)
	switch code {
	case "metadata_not_found", "metadata_revision_not_found":
		writeError(w, http.StatusNotFound, code)
		return
	}
	writeJSON(w, http.StatusOK, metadataResponse{
		CID:         cid,
		Revision:    rev.revision,
		ContentType: rev.contentType,
		Labels:      rev.labels,
		UpdatedAt:   rev.updatedAt,
	})
}

// parseMetadataPut fully validates a write body. Structural problems — not a
// single strict JSON document, missing, unknown or duplicate fields, wrong
// types, a non-integer expectedRevision — map to 400 invalid_request; value
// rule violations map to 422 invalid_metadata.
func parseMetadataPut(body []byte) (expected int64, contentType *string, labels map[string]string, code string, status int) {
	invalid := func() (int64, *string, map[string]string, string, int) {
		return 0, nil, nil, "invalid_request", http.StatusBadRequest
	}
	badValue := func() (int64, *string, map[string]string, string, int) {
		return 0, nil, nil, "invalid_metadata", http.StatusUnprocessableEntity
	}

	// wellFormedJSON has already rejected duplicate keys, so a map decode
	// preserves the field set exactly; a pointer struct could not, because
	// an explicit null decodes to the same nil pointer as a missing field.
	if !wellFormedJSON(body) {
		return invalid()
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return invalid()
	}
	for key := range doc {
		switch key {
		case "expectedRevision", "contentType", "labels":
		default:
			return invalid()
		}
	}
	expRaw, ok := doc["expectedRevision"]
	if !ok {
		return invalid()
	}
	ctRaw, ok := doc["contentType"]
	if !ok {
		return invalid()
	}
	labelsRaw, ok := doc["labels"]
	if !ok {
		return invalid()
	}

	expected, ok = parseNonNegativeDecimal(string(expRaw))
	if !ok {
		return invalid()
	}

	if string(ctRaw) == "null" {
		contentType = nil
	} else {
		var s string
		if err := json.Unmarshal(ctRaw, &s); err != nil {
			return invalid()
		}
		contentType = &s
	}

	// Labels must be a JSON object with string values; null is a type error.
	// An empty object decodes to a nil map, which is normalized so the
	// response encodes {} rather than null.
	if string(labelsRaw) == "null" {
		return invalid()
	}
	var labelMap map[string]string
	if err := json.Unmarshal(labelsRaw, &labelMap); err != nil {
		return invalid()
	}
	if labelMap == nil {
		labelMap = map[string]string{}
	}

	if contentType != nil && !validContentTypeValue(*contentType) {
		return badValue()
	}
	if len(labelMap) > maxLabels {
		return badValue()
	}
	for key, value := range labelMap {
		if !validLabelKey(key) || !validLabelValue(value) {
			return badValue()
		}
	}
	return expected, contentType, labelMap, "", 0
}

// commitMetadata appends one revision under the write lock, so the existence
// check, the revision check and the append are atomic: of concurrent writes
// naming the same expectedRevision exactly one commits. The object must
// exist; metadata never creates pins, audit events or statistics entries.
func (s *store) commitMetadata(cid string, expected int64, contentType *string, labels map[string]string, now time.Time) (rev metadataRevision, created bool, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[cid]; !exists {
		return metadataRevision{}, false, "object_not_found"
	}
	h := s.metadata[cid]
	var current int64
	if h != nil {
		current = int64(len(h.revisions))
	}
	if expected != current {
		return metadataRevision{}, false, "revision_conflict"
	}
	rev = metadataRevision{
		revision:    current + 1,
		contentType: contentType,
		labels:      labels,
		updatedAt:   now,
	}
	if h == nil {
		h = &metadataHistory{}
		s.metadata[cid] = h
	}
	h.revisions = append(h.revisions, rev)
	return rev, current == 0, ""
}

// metadataAt returns the current revision when revision is 0, or the named
// historical revision otherwise. The code is empty on success,
// metadata_not_found when the object has no metadata at all, and
// metadata_revision_not_found when the named revision is beyond the history.
func (s *store) metadataAt(cid string, revision int64) (metadataRevision, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.metadata[cid]
	if h == nil || len(h.revisions) == 0 {
		return metadataRevision{}, "metadata_not_found"
	}
	if revision == 0 {
		return h.revisions[len(h.revisions)-1], ""
	}
	if revision > int64(len(h.revisions)) {
		return metadataRevision{}, "metadata_revision_not_found"
	}
	return h.revisions[revision-1], ""
}

// parseNonNegativeDecimal parses a non-negative decimal integer with no
// sign, fraction or exponent. A digit string beyond int64 range is still a
// non-negative integer, so it is saturated to a value that simply matches no
// current revision rather than reported as a type error.
func parseNonNegativeDecimal(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return v, true
}

// parsePositiveDecimal parses a positive decimal integer with no sign,
// fraction or exponent; anything else, including zero and values beyond
// int64 range, is rejected.
func parsePositiveDecimal(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// validContentTypeValue reports whether s is 1 to 255 Unicode code points
// and free of control characters.
func validContentTypeValue(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 255 {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// validLabelKey reports whether s matches [a-z0-9][a-z0-9._-]{0,63}.
func validLabelKey(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// validLabelValue reports whether s is at most 256 Unicode code points and
// free of control characters.
func validLabelValue(s string) bool {
	if utf8.RuneCountInString(s) > 256 {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}
