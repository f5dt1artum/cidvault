package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// gatewayPrefix anchors every read-only gateway route. The gateway is
// dispatched on the raw request target before ServeMux path cleaning, since
// the mux would rewrite or redirect raw ".", ".." and empty segments and its
// PathValue results are already percent-decoded, which would lose the exact
// single-decoding semantics the gateway contract requires.
const gatewayPrefix = "/v1/gateway/"

// directoryMediaType is the media type of directory documents both when
// they are stored (as ordinary octet-stream objects) and when the gateway
// serves one back.
const directoryMediaType = "application/vnd.cidvault.directory+json"

// directoryVersion is the only directory format version this server
// understands.
const directoryVersion = 1

// maxDirectoryEntries bounds the number of entries one directory object may
// carry.
const maxDirectoryEntries = 4096

// maxGatewaySegments bounds the number of path segments resolved in one
// gateway request, excluding the root CID.
const maxGatewaySegments = 64

// maxNameRunes bounds a single directory entry name in Unicode code points.
const maxNameRunes = 255

// directoryEntryJSON is one strictly decoded directory entry. Pointer
// fields distinguish a missing key from a zero value.
type directoryEntryJSON struct {
	Name *string `json:"name"`
	Type *string `json:"type"`
	CID  *string `json:"cid"`
}

// directoryJSON is the strictly decoded directory document. Top level may
// carry exactly the version and entries keys.
type directoryJSON struct {
	Version *json.RawMessage      `json:"version"`
	Entries *[]directoryEntryJSON `json:"entries"`
}

// directoryEntry is one validated directory listing entry.
type directoryEntry struct {
	name string
	typ  string
	cid  string
}

// gateway handles every route under /v1/gateway/, including ones ServeMux
// would otherwise rewrite (raw dot-segments or empty segments), so it must
// parse the raw request target itself.
func (s *store) gateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", allowGetHead)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Method == http.MethodHead {
		w = headWriter{w}
	}
	rest, ok := gatewayRawPath(r)
	if !ok {
		// In principle the dispatcher only routes matching targets here.
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}
	rawSegments := strings.Split(rest, "/")
	rootCID, ok := decodeGatewaySegment(rawSegments[0])
	if !ok || !validCID(rootCID) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	rawPath := rawSegments[1:]
	if len(rawPath) > maxGatewaySegments {
		writeError(w, http.StatusBadRequest, "invalid_path")
		return
	}
	segments := make([]string, len(rawPath))
	for i, raw := range rawPath {
		seg, ok := decodeGatewaySegment(raw)
		if !ok || !utf8.ValidString(seg) || seg == "" ||
			strings.ContainsAny(seg, `/\`) || seg == "." || seg == ".." {
			writeError(w, http.StatusBadRequest, "invalid_path")
			return
		}
		segments[i] = seg
	}

	outcome := s.resolveGateway(rootCID, segments)
	if outcome.status != 0 {
		writeError(w, outcome.status, outcome.code)
		return
	}
	mediaType := "application/octet-stream"
	if outcome.directory {
		mediaType = directoryMediaType
	}
	serveBytes(w, r, outcome.obj.body(), mediaType, strongETag(outcome.obj.cid))
}

// gatewayOutcome is either a successful resolution (obj plus whether it is a
// directory) or the error code and status to report.
type gatewayOutcome struct {
	obj       *object
	directory bool
	code      string
	status    int
}

// resolveGateway walks from the root CID through each path segment against a
// single consistent snapshot of the object store: the read lock is held for
// the whole walk, so concurrent garbage collection can never make one
// request observe objects from different points in time. It never initiates
// a remote retrieval; locally absent referenced objects fail the request.
func (s *store) resolveGateway(rootCID string, segments []string) gatewayOutcome {
	s.mu.RLock()
	defer s.mu.RUnlock()

	root, exists := s.objects[rootCID]
	if !exists {
		return gatewayOutcome{code: "object_not_found", status: http.StatusNotFound}
	}
	entries, valid := parseDirectory(root.body())
	if !valid {
		return gatewayOutcome{code: "invalid_directory", status: http.StatusUnprocessableEntity}
	}
	current := root
	for i, name := range segments {
		entry, found := entries[name]
		if !found {
			return gatewayOutcome{code: "path_not_found", status: http.StatusNotFound}
		}
		last := i == len(segments)-1
		if entry.typ == "file" {
			// Descending through a file is a path-type conflict reported from
			// the directory metadata alone; the file object need not exist.
			if !last {
				return gatewayOutcome{code: "not_directory", status: http.StatusConflict}
			}
			target := s.objects[entry.cid]
			if target == nil {
				return gatewayOutcome{code: "gateway_target_missing", status: http.StatusFailedDependency}
			}
			return gatewayOutcome{obj: target, directory: false}
		}
		target := s.objects[entry.cid]
		if target == nil {
			return gatewayOutcome{code: "gateway_target_missing", status: http.StatusFailedDependency}
		}
		childEntries, valid := parseDirectory(target.body())
		if !valid {
			return gatewayOutcome{code: "invalid_directory", status: http.StatusUnprocessableEntity}
		}
		if last {
			return gatewayOutcome{obj: target, directory: true}
		}
		current = target
		entries = childEntries
	}
	return gatewayOutcome{obj: current, directory: true}
}

// body concatenates the object's chunks back into the stored byte sequence.
// Object bytes are immutable once stored.
func (o *object) body() []byte {
	if len(o.chunks) == 0 {
		return []byte{}
	}
	if len(o.chunks) == 1 {
		return o.chunks[0]
	}
	return bytes.Join(o.chunks, nil)
}

// parseDirectory fully validates one directory document and returns its
// entries indexed by name. A directory object is strict UTF-8 JSON whose top
// level carries exactly version (fixed at 1) and entries (at most 4096);
// every entry carries exactly name, type and cid, names are unique and
// strictly increasing by Unicode code point, each name is one path segment
// of 1..255 code points without "/", "\\", "."/"..", NUL or other control
// characters, type is file or directory, and cid has the existing shape.
func parseDirectory(body []byte) (map[string]directoryEntry, bool) {
	if !utf8.Valid(body) || !wellFormedJSON(body) {
		return nil, false
	}
	var doc directoryJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if doc.Version == nil || doc.Entries == nil || *doc.Entries == nil {
		return nil, false
	}
	version, err := parseInt(*doc.Version)
	if err != nil || version != directoryVersion {
		return nil, false
	}
	raw := *doc.Entries
	if len(raw) > maxDirectoryEntries {
		return nil, false
	}
	entries := make(map[string]directoryEntry, len(raw))
	prev := ""
	for i := range raw {
		e := raw[i]
		if e.Name == nil || e.Type == nil || e.CID == nil {
			return nil, false
		}
		name := *e.Name
		if !validDirectoryName(name) || !runesLess(prev, name) {
			return nil, false
		}
		if *e.Type != "file" && *e.Type != "directory" {
			return nil, false
		}
		if !validCID(*e.CID) {
			return nil, false
		}
		entries[name] = directoryEntry{name: name, typ: *e.Type, cid: *e.CID}
		prev = name
	}
	return entries, true
}

// validDirectoryName reports whether name satisfies the entry-name contract:
// 1..255 Unicode code points, neither "." nor "..", without "/", "\\", NUL
// or any Cc control character.
func validDirectoryName(name string) bool {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > maxNameRunes || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// runesLess reports whether a sorts strictly before b by Unicode code point.
// UTF-8 preserves code-point order byte-wise, but the comparison is done on
// runes to state the contract directly. Inputs are already valid UTF-8.
func runesLess(a, b string) bool {
	for a != "" && b != "" {
		ra, sizeA := utf8.DecodeRuneInString(a)
		rb, sizeB := utf8.DecodeRuneInString(b)
		if ra != rb {
			return ra < rb
		}
		a = a[sizeA:]
		b = b[sizeB:]
	}
	return a == "" && b != ""
}

// decodeGatewaySegment percent-decodes exactly one raw path segment, exactly
// once: every "%" must be followed by two hex digits, "+" is literal and
// there is no recursive decoding. The caller is responsible for the UTF-8
// and character checks on the result.
func decodeGatewaySegment(seg string) (string, bool) {
	if !strings.ContainsRune(seg, '%') {
		return seg, true
	}
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(seg) {
			return "", false
		}
		hi, okHi := hexValue(seg[i+1])
		lo, okLo := hexValue(seg[i+2])
		if !okHi || !okLo {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), true
}

// hexValue converts one hexadecimal digit to its value.
func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// gatewayRawPath returns the percent-encoded request-target path after
// gatewayPrefix. The raw target is used so escaped slashes and dots are
// preserved exactly as sent; ServeMux never sees gateway requests.
func gatewayRawPath(r *http.Request) (string, bool) {
	target := r.RequestURI
	if target == "" {
		target = r.URL.EscapedPath()
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	if !strings.HasPrefix(target, gatewayPrefix) {
		return "", false
	}
	return target[len(gatewayPrefix):], true
}
