package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// headWriter discards response bodies for HEAD requests while preserving
// the status code and headers, so error paths shared with GET never write a
// body for any status.
type headWriter struct{ http.ResponseWriter }

func (headWriter) Write(p []byte) (int, error) { return len(p), nil }

// allowGetHead is the Allow header value for the read-only byte-serving
// entries (objects, blocks, gateway), which accept GET and HEAD only.
const allowGetHead = "GET, HEAD"

// strongETag renders the strong ETag for a content identifier: the full CID
// wrapped in double quotes.
func strongETag(cid string) string {
	return `"` + cid + `"`
}

// serveBytes answers a GET or HEAD request for a fixed payload with
// conditional and single-range support layered on the plain full-body
// response. Evaluation order is If-None-Match first (a match yields a
// bodyless 304, an unparseable value a 400), then Range (a satisfiable
// single bytes range yields 206, anything else a 416). HEAD ignores Range
// and reports the full response headers without ever writing a body. The
// caller is responsible for method, path and resource checks beforehand.
func serveBytes(w http.ResponseWriter, r *http.Request, body []byte, contentType, etag string) {
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Accept-Ranges", "bytes")

	if values, present := r.Header["If-None-Match"]; present {
		matched, ok := evalIfNoneMatch(values, etag)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if matched {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	h.Set("Content-Type", contentType)
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		return
	}

	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseSingleRange(rng, len(body))
		if !ok {
			h.Set("Content-Range", "bytes */"+strconv.Itoa(len(body)))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable")
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		h.Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start : end+1])
		return
	}

	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// evalIfNoneMatch applies If-None-Match with weak comparison against etag.
// The accepted shapes are "*", a single entity tag, or a comma-separated
// list of entity tags (each optionally carrying the weak "W/" prefix); any
// match reports matched. A value that cannot be parsed into one of these
// forms reports ok == false.
func evalIfNoneMatch(values []string, etag string) (matched, ok bool) {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			tag := strings.TrimSpace(part)
			if tag == "" {
				return false, false
			}
			if tag == "*" {
				return true, true
			}
			opaque := tag
			if strings.HasPrefix(opaque, "W/") {
				opaque = opaque[2:]
			}
			if len(opaque) < 2 || opaque[0] != '"' || opaque[len(opaque)-1] != '"' ||
				!validETagContents(opaque[1:len(opaque)-1]) {
				return false, false
			}
			// Weak comparison ignores the weakness indicator, so a "W/"
			// prefixed tag matches the strong etag by opaque value.
			if opaque == etag {
				return true, true
			}
		}
	}
	return false, true
}

// validETagContents reports whether s is a legal opaque-tag body: visible
// ASCII except the double quote, plus obs-text bytes.
func validETagContents(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x21 || (c >= 0x23 && c <= 0x7e) || c >= 0x80 {
			continue
		}
		return false
	}
	return true
}

// parseSingleRange parses a Range header holding exactly one bytes range
// against a body of size bytes and returns the satisfiable closed interval.
// It reports ok == false for a non-bytes unit, malformed syntax, multiple
// ranges, a start beyond the body, an end before the start, a zero-length
// suffix, or any range on an empty body. Ends and suffixes past the body
// are truncated to the body bounds.
func parseSingleRange(header string, size int) (start, end int, ok bool) {
	if size == 0 {
		return 0, 0, false
	}
	eq := strings.IndexByte(header, '=')
	if eq < 0 || !strings.EqualFold(strings.TrimSpace(header[:eq]), "bytes") {
		return 0, 0, false
	}
	spec := header[eq+1:]
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	spec = strings.TrimSpace(spec)
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	first, second := spec[:dash], spec[dash+1:]
	if first == "" {
		// Suffix form: the final n bytes, truncated to the whole body.
		n, valid := parseUintSaturated(second)
		if !valid || n == 0 {
			return 0, 0, false
		}
		if n >= int64(size) {
			return 0, size - 1, true
		}
		return size - int(n), size - 1, true
	}
	from, valid := parseUintSaturated(first)
	if !valid {
		return 0, 0, false
	}
	to := int64(size) - 1
	if second != "" {
		to, valid = parseUintSaturated(second)
		if !valid {
			return 0, 0, false
		}
	}
	if from >= int64(size) || to < from {
		return 0, 0, false
	}
	if to >= int64(size) {
		to = int64(size) - 1
	}
	return int(from), int(to), true
}

// parseUintSaturated parses a non-empty string of decimal digits, saturating
// at math.MaxInt64 so absurdly large offsets behave as "beyond the body"
// instead of overflowing.
func parseUintSaturated(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (math.MaxInt64-9)/10 {
			n = math.MaxInt64
		} else {
			n = n*10 + int64(c-'0')
		}
	}
	return n, true
}
