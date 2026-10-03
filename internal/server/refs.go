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
	"strings"
	"time"
)

// refsPrefix anchors every named-reference route. Like the gateway, refs
// are dispatched on the raw request target before ServeMux path cleaning,
// so the name undergoes exactly one percent decode and an encoded slash or
// malformed escape cannot be silently rewritten into another route.
const refsPrefix = "/v1/refs/"

// refObjectName is the fixed trailing segment of the object endpoint.
const refObjectName = "object"

// refNamePattern matches the only legal decoded reference name: a lowercase
// alphanumeric first character followed by up to 127 lowercase
// alphanumeric characters, dots, underscores or hyphens.
var refNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// refPutRequest is the strictly decoded PUT body. RawMessage fields stay
// nil for missing keys but hold the literal "null" for JSON nulls, so the
// two cases stay distinguishable.
type refPutRequest struct {
	CID              json.RawMessage `json:"cid"`
	ExpectedRevision json.RawMessage `json:"expectedRevision"`
}

// refResponse is the JSON body of ref reads and writes.
type refResponse struct {
	Name      string    `json:"name"`
	Revision  int64     `json:"revision"`
	CID       string    `json:"cid"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// refsDispatch handles every route under /v1/refs/, parsing the raw
// request target itself for the same single-decoding reasons as the
// gateway. The shape is either "/v1/refs/{name}" or
// "/v1/refs/{name}/object"; anything else is no route. The raw query
// string is carried through verbatim so handlers never depend on r.URL,
// which is unset for raw-target requests.
func (s *store) refsDispatch(w http.ResponseWriter, r *http.Request) {
	target := r.RequestURI
	if target == "" {
		target = r.URL.EscapedPath()
	}
	rawPath, rawQuery, _ := strings.Cut(target, "?")
	rest := rawPath[len(refsPrefix):]
	segments := strings.Split(rest, "/")
	switch len(segments) {
	case 1:
		s.refByID(w, r, segments[0], rawQuery)
	case 2:
		if segments[1] != refObjectName {
			http.NotFound(w, r)
			return
		}
		s.refObjectEntry(w, r, segments[0], rawQuery)
	default:
		http.NotFound(w, r)
	}
}

// decodeRefName percent-decodes a single raw name segment exactly once and
// validates it against the name contract.
func decodeRefName(raw string) (string, bool) {
	name, ok := decodeGatewaySegment(raw)
	if !ok || !refNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}

// refByID handles GET and PUT on /v1/refs/{name}.
func (s *store) refByID(w http.ResponseWriter, r *http.Request, rawName, rawQuery string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	name, ok := decodeRefName(rawName)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_ref")
		return
	}
	if r.Method == http.MethodPut {
		s.putRef(w, r, name)
		return
	}
	s.getRef(w, rawQuery, name)
}

// putRef handles PUT /v1/refs/{name}.
func (s *store) putRef(w http.ResponseWriter, r *http.Request, name string) {
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
	var req refPutRequest
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
	expected, err := parseInt(req.ExpectedRevision)
	if err != nil || expected < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var cidPtr *string
	if err := json.Unmarshal(req.CID, &cidPtr); err != nil || cidPtr == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validCID(*cidPtr) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	cid := *cidPtr
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
func (s *store) getRef(w http.ResponseWriter, rawQuery, name string) {
	revision, ok := parseRefRevisionQuery(rawQuery)
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

// refObjectEntry handles GET on /v1/refs/{name}/object.
func (s *store) refObjectEntry(w http.ResponseWriter, r *http.Request, rawName, rawQuery string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	name, ok := decodeRefName(rawName)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_ref")
		return
	}
	revision, ok := parseRefRevisionQuery(rawQuery)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	rev, obj, code := s.resolveRefObject(name, revision)
	if code != "" {
		status := http.StatusFailedDependency
		if code != "reference_target_missing" {
			status = http.StatusNotFound
		}
		writeError(w, status, code)
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

// parseRefRevisionQuery parses the optional single positive-decimal
// revision query parameter. No parameters means the current revision
// (zero); anything else (unknown keys, repeated values, non-integers,
// zero or negative) is invalid.
func parseRefRevisionQuery(rawQuery string) (int64, bool) {
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

// writeRef encodes one ref revision.
func writeRef(w http.ResponseWriter, status int, name string, rev refRevision) {
	writeJSON(w, status, refResponse{
		Name:      name,
		Revision:  rev.revision,
		CID:       rev.cid,
		UpdatedAt: rev.updatedAt,
	})
}
