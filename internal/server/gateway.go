package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// directoryMediaType is the media type of stored directory documents and
	// of gateway responses that resolve to a directory.
	directoryMediaType = "application/vnd.cidvault.directory+json"

	// directoryVersion is the only directory document version understood.
	directoryVersion = 1

	// maxDirectoryEntries bounds the entries array of a directory document.
	maxDirectoryEntries = 4096

	// maxDirectoryNameRunes bounds a single entry name in Unicode code
	// points.
	maxDirectoryNameRunes = 255

	// maxGatewaySegments bounds the number of path segments one gateway
	// request resolves.
	maxGatewaySegments = 64

	// gatewayPathPrefix covers both gateway routes; the handler parses the
	// remainder itself so the mux's path cleaning cannot redirect requests
	// the contract answers with invalid_path.
	gatewayPathPrefix = "/v1/gateway/"
)

// directoryEntryJSON is one entry of a validated directory document. The
// slice keeps stored order, which is strictly ascending by name.
type directoryEntryJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
	CID  string `json:"cid"`
}

// directoryDocument is the validated shape of a stored directory body. The
// gateway always serves the original stored bytes; this structure only
// drives resolution and contract checks.
type directoryDocument struct {
	Version int
	Entries []directoryEntryJSON
}

// parseDirectoryDocument validates body against the directory contract: a
// single strict JSON object with exactly version and entries; version fixed
// at 1; at most maxDirectoryEntries entries, each holding exactly name,
// type and cid, with a legal single-segment name, strictly ascending and
// unique names, type file or directory, and a well-formed cid.
func parseDirectoryDocument(body []byte) (*directoryDocument, bool) {
	if !wellFormedJSON(body) {
		return nil, false
	}
	var doc struct {
		Version *json.RawMessage   `json:"version"`
		Entries *[]json.RawMessage `json:"entries"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if doc.Version == nil || doc.Entries == nil {
		return nil, false
	}
	version, err := parseInt(*doc.Version)
	if err != nil || version != directoryVersion {
		return nil, false
	}
	rawEntries := *doc.Entries
	if len(rawEntries) > maxDirectoryEntries {
		return nil, false
	}
	entries := make([]directoryEntryJSON, 0, len(rawEntries))
	prev := ""
	for _, raw := range rawEntries {
		if !wellFormedJSON(raw) {
			return nil, false
		}
		var e struct {
			Name *string `json:"name"`
			Type *string `json:"type"`
			CID  *string `json:"cid"`
		}
		edec := json.NewDecoder(bytes.NewReader(raw))
		edec.DisallowUnknownFields()
		if err := edec.Decode(&e); err != nil {
			return nil, false
		}
		// A null value leaves the pointer nil just like a missing key.
		if e.Name == nil || e.Type == nil || e.CID == nil {
			return nil, false
		}
		if !validDirectoryName(*e.Name) {
			return nil, false
		}
		// UTF-8 byte order equals code-point order, so the string
		// comparison enforces strictly ascending, unique names.
		if *e.Name <= prev {
			return nil, false
		}
		if *e.Type != "file" && *e.Type != "directory" {
			return nil, false
		}
		if !validCID(*e.CID) {
			return nil, false
		}
		prev = *e.Name
		entries = append(entries, directoryEntryJSON{Name: *e.Name, Type: *e.Type, CID: *e.CID})
	}
	return &directoryDocument{Version: version, Entries: entries}, true
}

// validDirectoryName reports whether name is one path segment of 1 to 255
// Unicode code points: not "." or "..", free of slash, backslash, NUL and
// ASCII control characters (including DEL). The input is already valid
// UTF-8 because it came through JSON decoding.
func validDirectoryName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	n := 0
	for _, r := range name {
		n++
		if n > maxDirectoryNameRunes {
			return false
		}
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return n >= 1
}

// gateway handles GET on /v1/gateway/{cid} and
// /v1/gateway/{cid}/{path...}. It never initiates remote retrieval and
// records no audit events.
func (s *store) gateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Parse the still-escaped path ourselves: splitting before decoding is
	// what makes an encoded slash a literal part of a segment, and decoding
	// each segment exactly once forbids the double-encoded form.
	cid, segments, code := parseGatewayPath(strings.TrimPrefix(r.URL.EscapedPath(), gatewayPathPrefix))
	if code != "" {
		writeError(w, http.StatusBadRequest, code)
		return
	}
	s.resolveGateway(w, cid, segments)
}

// parseGatewayPath splits the escaped suffix after "/v1/gateway/" into the
// root cid and at most maxGatewaySegments decoded path segments. Every
// segment is percent-decoded exactly once, then matched literally with no
// Unicode normalization. It returns the error code "invalid_cid" or
// "invalid_path" for every malformed input.
func parseGatewayPath(rest string) (cid string, segments []string, code string) {
	cidEsc, pathEsc, hasPath := strings.Cut(rest, "/")
	cid, err := url.PathUnescape(cidEsc)
	if err != nil || !validCID(cid) {
		return "", nil, "invalid_cid"
	}
	if !hasPath {
		return cid, []string{}, ""
	}
	rawSegments := strings.Split(pathEsc, "/")
	if len(rawSegments) > maxGatewaySegments {
		return "", nil, "invalid_path"
	}
	segments = make([]string, 0, len(rawSegments))
	for _, raw := range rawSegments {
		seg, err := url.PathUnescape(raw)
		if err != nil || seg == "" || seg == "." || seg == ".." ||
			strings.ContainsAny(seg, `/\`) || !utf8.ValidString(seg) {
			return "", nil, "invalid_path"
		}
		segments = append(segments, seg)
	}
	return cid, segments, ""
}

// resolveGateway walks one directory object per segment against a single
// point-in-time snapshot of the object store, so concurrent uploads or
// garbage collection cannot mix objects from different instants.
func (s *store) resolveGateway(w http.ResponseWriter, rootCID string, segments []string) {
	snap := s.snapshotObjects()

	current, ok := snap[rootCID]
	if !ok {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	for i, seg := range segments {
		doc, valid := parseDirectoryDocument(objectBytes(current))
		if !valid {
			writeError(w, http.StatusUnprocessableEntity, "invalid_directory")
			return
		}
		var entry *directoryEntryJSON
		for j := range doc.Entries {
			if doc.Entries[j].Name == seg {
				entry = &doc.Entries[j]
				break
			}
		}
		if entry == nil {
			writeError(w, http.StatusNotFound, "path_not_found")
			return
		}
		last := i == len(segments)-1
		if !last {
			// Descending through anything but a directory is a structural
			// error regardless of whether the referenced object exists.
			if entry.Type != "directory" {
				writeError(w, http.StatusConflict, "not_directory")
				return
			}
			next, exists := snap[entry.CID]
			if !exists {
				writeError(w, http.StatusFailedDependency, "gateway_target_missing")
				return
			}
			current = next
			continue
		}
		target, exists := snap[entry.CID]
		if !exists {
			writeError(w, http.StatusFailedDependency, "gateway_target_missing")
			return
		}
		if entry.Type == "file" {
			writeStoredObject(w, target, "application/octet-stream")
			return
		}
		// Read explicitly as a directory: its body must honor the contract.
		if _, valid := parseDirectoryDocument(objectBytes(target)); !valid {
			writeError(w, http.StatusUnprocessableEntity, "invalid_directory")
			return
		}
		writeStoredObject(w, target, directoryMediaType)
		return
	}

	// No path: the root must itself be a valid directory.
	if _, valid := parseDirectoryDocument(objectBytes(current)); !valid {
		writeError(w, http.StatusUnprocessableEntity, "invalid_directory")
		return
	}
	writeStoredObject(w, current, directoryMediaType)
}

// snapshotObjects copies the root-identifier-to-object mapping at one
// instant. Objects are immutable after insertion, so resolution against
// the copy stays consistent while concurrent requests put new objects or
// garbage-collect existing ones.
func (s *store) snapshotObjects() map[string]*object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := make(map[string]*object, len(s.objects))
	for cid, obj := range s.objects {
		snap[cid] = obj
	}
	return snap
}

// objectBytes reconstructs a stored object's raw body from its chunks.
// Directory bodies are small under the entry cap, and the common case is a
// single chunk which needs no copy.
func objectBytes(obj *object) []byte {
	switch len(obj.chunks) {
	case 0:
		return nil
	case 1:
		return obj.chunks[0]
	default:
		return bytes.Join(obj.chunks, nil)
	}
}

// writeStoredObject streams the original stored bytes with an exact
// Content-Length and the given media type.
func writeStoredObject(w http.ResponseWriter, obj *object, mediaType string) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(obj.size))
	for _, chunk := range obj.chunks {
		_, _ = w.Write(chunk)
	}
}
