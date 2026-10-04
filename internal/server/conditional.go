package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// readAllow is the Allow header value for the object, block and gateway read
// endpoints, which accept exactly GET and HEAD.
const readAllow = "GET, HEAD"

// readMethodAllowed reports whether the request method is one of the read
// methods; otherwise it writes the shared 405 response and returns false.
func readMethodAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", readAllow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	return false
}

// writeReadError writes an error response for a read endpoint. HEAD requests
// get the status and headers the GET error response would carry, but never a
// body, whatever the status.
func writeReadError(w http.ResponseWriter, r *http.Request, status int, code string) {
	if r.Method == http.MethodHead {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code}})
		w.Header().Set("Content-Type", "application/json")
		// json.Encoder appends a newline to the marshalled document.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
		w.WriteHeader(status)
		return
	}
	writeError(w, status, code)
}

// serveStoredContent writes the conditional, range-aware read response shared
// by the object, block and gateway read endpoints. Resource validation must
// already have happened: If-None-Match is evaluated first (a match yields a
// bodiless 304, an unparseable value a 400), then Range for GET (HEAD ignores
// it). cid names the content behind the strong ETag. writeRange writes the
// closed interval [start, end] of the body; HEAD responses carry the full
// response headers but no body.
func serveStoredContent(w http.ResponseWriter, r *http.Request, cid, mediaType string, size int, writeRange func(io.Writer, int, int)) {
	etag := `"` + cid + `"`
	head := r.Method == http.MethodHead
	h := w.Header()
	if values, present := r.Header["If-None-Match"]; present {
		match, ok := evalIfNoneMatch(strings.Join(values, ","), etag)
		if !ok {
			writeReadError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		if match {
			h.Set("ETag", etag)
			h.Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("ETag", etag)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", mediaType)
	if !head {
		if values, present := r.Header["Range"]; present {
			if len(values) != 1 {
				writeRangeNotSatisfiable(w, r, size)
				return
			}
			start, end, ok := parseByteRange(values[0], size)
			if !ok {
				writeRangeNotSatisfiable(w, r, size)
				return
			}
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			h.Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			writeRange(w, start, end)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(size))
	w.WriteHeader(http.StatusOK)
	if !head && size > 0 {
		writeRange(w, 0, size-1)
	}
}

// writeRangeNotSatisfiable writes the shared 416 response: the complete
// length is always reported as "bytes */<size>".
func writeRangeNotSatisfiable(w http.ResponseWriter, r *http.Request, size int) {
	w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(size))
	writeReadError(w, r, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
}

// evalIfNoneMatch evaluates an If-None-Match value against the current strong
// ETag using weak comparison: a bare "*" matches, and any entity-tag in the
// comma-separated list whose opaque tag equals the current one matches
// regardless of a "W/" prefix. ok is false when the value is neither "*" nor
// a list of entity-tags; an empty value is an empty list and matches nothing.
func evalIfNoneMatch(value, etag string) (match, ok bool) {
	if strings.TrimSpace(value) == "" {
		return false, true
	}
	if strings.TrimSpace(value) == "*" {
		return true, true
	}
	// etag is `"<opaque>"`; weak comparison matches on the opaque tag.
	opaque := etag[1 : len(etag)-1]
	for _, part := range strings.Split(value, ",") {
		tag, valid := parseEntityTag(strings.TrimSpace(part))
		if !valid {
			return false, false
		}
		if tag == opaque {
			return true, true
		}
	}
	return false, true
}

// parseEntityTag parses one entity-tag — an optional case-sensitive "W/"
// weakness prefix followed by a quoted opaque tag — and returns the opaque
// tag without its quotes.
func parseEntityTag(s string) (string, bool) {
	s = strings.TrimPrefix(s, "W/")
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	inner := s[1 : len(s)-1]
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		// etagc = %x21 / %x23-7E / obs-text; DQUOTE and controls are excluded.
		if c == 0x21 || (c >= 0x23 && c <= 0x7e) || c >= 0x80 {
			continue
		}
		return "", false
	}
	return inner, true
}

// parseByteRange parses a Range header carrying exactly one bytes range
// against a body of the given size and returns the satisfiable closed
// interval. Ends past the body and overlong suffixes clip to the body. Any
// deviation — another unit, bad syntax, multiple ranges, a start beyond the
// body, an end before the start, a zero-length suffix, or any range on an
// empty body — is unsatisfiable.
func parseByteRange(header string, size int) (start, end int, ok bool) {
	if size == 0 {
		return 0, 0, false
	}
	if len(header) < len("bytes=") || !strings.EqualFold(header[:len("bytes=")], "bytes=") {
		return 0, 0, false
	}
	spec := header[len("bytes="):]
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, last := spec[:dash], spec[dash+1:]
	if first == "" {
		// Suffix form: the last n bytes; a zero-length suffix never satisfies.
		n, valid := parseRangeNumber(last)
		if !valid || n == 0 {
			return 0, 0, false
		}
		if n > int64(size) {
			n = int64(size)
		}
		return size - int(n), size - 1, true
	}
	s, valid := parseRangeNumber(first)
	if !valid {
		return 0, 0, false
	}
	e := int64(size) - 1
	if last != "" {
		if e, valid = parseRangeNumber(last); !valid {
			return 0, 0, false
		}
	}
	if s >= int64(size) || e < s {
		return 0, 0, false
	}
	if e > int64(size)-1 {
		e = int64(size) - 1
	}
	return int(s), int(e), true
}

// parseRangeNumber parses a non-empty run of decimal digits. Bodies never
// exceed MaxObjectSize, so values too long to matter are clamped to a
// still-huge sentinel instead of overflowing.
func parseRangeNumber(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	const huge = int64(1) << 62
	if len(s) > 18 {
		return huge, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return huge, true
	}
	return n, true
}
