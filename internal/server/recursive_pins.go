package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"sort"
	"time"
)

// maxRecursivePinDepth bounds the directory tree depth a recursive pin
// traverses. The root sits at depth 0; visiting any member deeper than this
// fails the request.
const maxRecursivePinDepth = 64

// maxRecursivePinMembers bounds the number of unique objects (root plus
// descendants) one recursive pin may cover.
const maxRecursivePinMembers = 10000

// recursivePin records a retention guarantee for a directory root and every
// object reachable from it. members holds the unique member identifiers
// (root included) sorted lexically, captured at commit time; bytes is the
// sum of their body lengths. A nil expiresAt means the pin is permanent.
// Recursive pins live in process memory only, take no part in the audit log
// and are not listed by GET /v1/pins.
type recursivePin struct {
	expiresAt *time.Time
	members   []string
	bytes     int
}

// recursivePinRequest is the JSON body accepted by
// PUT /v1/recursive-pins/{cid}. An absent or null expiresAt pins
// permanently.
type recursivePinRequest struct {
	ExpiresAt *string `json:"expiresAt"`
}

// recursivePinResponse is the JSON body returned by recursive pin creation,
// update and lookup. ExpiresAt is null for permanent pins; members carries
// the commit-time snapshot of unique member identifiers sorted by cid.
type recursivePinResponse struct {
	CID       string     `json:"cid"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Objects   int        `json:"objects"`
	Bytes     int        `json:"bytes"`
	Members   []string   `json:"members"`
}

// recursivePinByID handles GET, PUT and DELETE on /v1/recursive-pins/{cid}.
func (s *store) recursivePinByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.putRecursivePin(w, r)
	case http.MethodGet:
		s.getRecursivePin(w, r)
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
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	req, code := decodeRecursivePinRequest(body)
	if code != "" {
		writeError(w, http.StatusBadRequest, code)
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
	rp, created, code, status := s.setRecursivePin(cid, expiresAt, now)
	if code != "" {
		writeError(w, status, code)
		return
	}
	respStatus := http.StatusOK
	if created {
		respStatus = http.StatusCreated
	}
	writeJSON(w, respStatus, recursivePinResponse{
		CID:       cid,
		ExpiresAt: expiresAt,
		Objects:   len(rp.members),
		Bytes:     rp.bytes,
		Members:   rp.members,
	})
}

// getRecursivePin handles GET /v1/recursive-pins/{cid}.
func (s *store) getRecursivePin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	pin, ok := s.recursivePinAt(cid, time.Now())
	if !ok {
		writeError(w, http.StatusNotFound, "recursive_pin_not_found")
		return
	}
	writeJSON(w, http.StatusOK, recursivePinResponse{
		CID:       cid,
		ExpiresAt: pin.expiresAt,
		Objects:   len(pin.members),
		Bytes:     pin.bytes,
		Members:   pin.members,
	})
}

// deleteRecursivePin handles DELETE /v1/recursive-pins/{cid}. Deleting the
// record never deletes the member objects and is not audited.
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

// decodeRecursivePinRequest strictly decodes the request body: it must be
// empty or a single JSON object carrying only expiresAt, with no duplicate
// keys and no trailing data. The returned string is empty on success or the
// error code to report.
func decodeRecursivePinRequest(body []byte) (recursivePinRequest, string) {
	var req recursivePinRequest
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return req, ""
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return req, "invalid_request"
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return req, "invalid_request"
	}
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return req, "invalid_request"
		}
		key, ok := keyTok.(string)
		if !ok || key != "expiresAt" || seen[key] {
			return req, "invalid_request"
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return req, "invalid_request"
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return req, "invalid_request"
		}
		req.ExpiresAt = value
	}
	if _, err := dec.Token(); err != nil {
		return req, "invalid_request"
	}
	if _, err := dec.Token(); err != io.EOF {
		return req, "invalid_request"
	}
	return req, ""
}

// setRecursivePin validates the directory tree rooted at cid and commits the
// recursive pin atomically: the traversal, the existence checks and the
// commit all observe the same instant, so a garbage collection can neither
// interleave with the walk nor remove an object the new pin protects. A
// failed walk leaves no record behind. The returned string is empty on
// success or the error code to report with the given status:
// object_not_found, invalid_directory, recursive_pin_target_missing or
// recursive_pin_too_large. created is true when no valid recursive pin was
// in force for cid at now.
func (s *store) setRecursivePin(cid string, expiresAt *time.Time, now time.Time) (committed recursivePin, created bool, code string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	root, exists := s.objects[cid]
	if !exists {
		return recursivePin{}, false, "object_not_found", http.StatusNotFound
	}
	rootEntries, valid := parseDirectory(root.body())
	if !valid {
		return recursivePin{}, false, "invalid_directory", http.StatusUnprocessableEntity
	}

	// Breadth-first walk over the directory tree. Members are de-duplicated
	// by cid at enqueue time, so shared subtrees and reference cycles are
	// visited once and each node is reached at its shallowest depth.
	members := []string{cid}
	totalBytes := root.size
	visited := map[string]bool{cid: true}
	queue := []walkChild{}
	enqueue := func(entries map[string]directoryEntry, depth int) {
		for _, c := range sortedDirectoryChildren(entries, depth) {
			if !visited[c.cid] {
				visited[c.cid] = true
				queue = append(queue, c)
			}
		}
	}
	enqueue(rootEntries, 1)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if node.depth > maxRecursivePinDepth {
			return recursivePin{}, false, "recursive_pin_too_large", http.StatusUnprocessableEntity
		}
		obj := s.objects[node.cid]
		if obj == nil {
			return recursivePin{}, false, "recursive_pin_target_missing", http.StatusFailedDependency
		}
		members = append(members, node.cid)
		if len(members) > maxRecursivePinMembers {
			return recursivePin{}, false, "recursive_pin_too_large", http.StatusUnprocessableEntity
		}
		totalBytes += obj.size
		if !node.isDir {
			continue
		}
		entries, valid := parseDirectory(obj.body())
		if !valid {
			return recursivePin{}, false, "invalid_directory", http.StatusUnprocessableEntity
		}
		enqueue(entries, node.depth+1)
	}

	sort.Strings(members)
	existing, found := s.recursivePins[cid]
	created = !found || !validPin(pin{expiresAt: existing.expiresAt}, now)
	s.recursivePins[cid] = recursivePin{expiresAt: expiresAt, members: members, bytes: totalBytes}
	return s.recursivePins[cid], created, "", 0
}

// walkChild is one enqueued directory-tree node awaiting its visit.
type walkChild struct {
	cid   string
	depth int
	isDir bool
}

// sortedDirectoryChildren flattens directory entries into a slice ordered by
// entry name, so the traversal order is deterministic.
func sortedDirectoryChildren(entries map[string]directoryEntry, depth int) []walkChild {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	children := make([]walkChild, 0, len(names))
	for _, name := range names {
		e := entries[name]
		children = append(children, walkChild{cid: e.cid, depth: depth, isDir: e.typ == "directory"})
	}
	return children
}

// recursivePinAt returns the recursive pin in force for cid at now,
// reporting whether one was found. Expired records are treated as absent.
func (s *store) recursivePinAt(cid string, now time.Time) (recursivePin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, found := s.recursivePins[cid]
	if !found || !validPin(pin{expiresAt: p.expiresAt}, now) {
		return recursivePin{}, false
	}
	return p, true
}

// removeRecursivePin deletes the recursive pin for cid, reporting whether a
// valid one was in force at now. Expired records are removed lazily and
// reported as absent. The member objects are never touched and no audit
// event is recorded.
func (s *store) removeRecursivePin(cid string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, found := s.recursivePins[cid]
	if !found || !validPin(pin{expiresAt: p.expiresAt}, now) {
		delete(s.recursivePins, cid)
		return false
	}
	delete(s.recursivePins, cid)
	return true
}
