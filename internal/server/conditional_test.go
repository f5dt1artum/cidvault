package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// conditionalRequest issues a request with the given headers and returns the
// recorder.
func conditionalRequest(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestObjectGetConditionalHeaders(t *testing.T) {
	h := Handler()
	payload := []byte("conditional caching payload")
	cid := uploadBytes(t, h, payload)
	etag := `"` + cid + `"`

	rec := conditionalRequest(h, http.MethodGet, "/v1/objects/"+cid, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q", got, etag)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body mismatch")
	}

	// Each accepted If-None-Match form yields a bodyless 304.
	for _, value := range []string{"*", etag, "W/" + etag, `"other"`, etag + `, "zzz"`} {
		rec = conditionalRequest(h, http.MethodGet, "/v1/objects/"+cid, map[string]string{"If-None-Match": value})
		want := http.StatusOK
		if value != `"other"` {
			want = http.StatusNotModified
		}
		if rec.Code != want {
			t.Fatalf("If-None-Match %q: status = %d, want %d", value, rec.Code, want)
		}
		if want == http.StatusNotModified && rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q: 304 carried a body", value)
		}
	}

	// Unparseable If-None-Match values are rejected.
	for _, value := range []string{"", "abc", `"unterminated`, `W/"`} {
		rec = conditionalRequest(h, http.MethodGet, "/v1/objects/"+cid, map[string]string{"If-None-Match": value})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("If-None-Match %q: status = %d, want 400", value, rec.Code)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// A matching If-None-Match wins over Range.
	rec = conditionalRequest(h, http.MethodGet, "/v1/objects/"+cid, map[string]string{
		"If-None-Match": etag,
		"Range":         "bytes=0-3",
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match+Range: status = %d, want 304", rec.Code)
	}

	// Resource errors precede conditional evaluation.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = conditionalRequest(h, http.MethodGet, "/v1/objects/"+missing, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object with If-None-Match: status = %d, want 404", rec.Code)
	}
	assertErrorCode(t, rec, "object_not_found")
	rec = conditionalRequest(h, http.MethodGet, "/v1/objects/not-a-cid", map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cid with If-None-Match: status = %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_cid")
}

func TestObjectGetRange(t *testing.T) {
	h := Handler()
	payload := []byte("0123456789abcdef")
	cid := uploadBytes(t, h, payload)
	path := "/v1/objects/" + cid

	cases := []struct {
		rangeHeader string
		want        string
	}{
		{"bytes=0-4", "01234"},
		{"bytes=4-", "456789abcdef"},
		{"bytes=-4", "cdef"},
		{"bytes=0-15", "0123456789abcdef"}, // full body is still 206
		{"bytes=10-100", "abcdef"},         // end truncated to the body
		{"bytes=-100", "0123456789abcdef"}, // suffix truncated to the body
		{"bytes=5-5", "5"},
	}
	for _, tc := range cases {
		rec := conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": tc.rangeHeader})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("%s: status = %d, want 206", tc.rangeHeader, rec.Code)
		}
		if got := rec.Body.String(); got != tc.want {
			t.Fatalf("%s: body = %q, want %q", tc.rangeHeader, got, tc.want)
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(tc.want)) {
			t.Fatalf("%s: Content-Length = %q, want %d", tc.rangeHeader, got, len(tc.want))
		}
		if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Fatalf("%s: Accept-Ranges = %q, want bytes", tc.rangeHeader, got)
		}
	}

	// Content-Range reflects the actual closed interval and full length.
	rec := conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": "bytes=4-"})
	if got := rec.Header().Get("Content-Range"); got != "bytes 4-15/16" {
		t.Fatalf("Content-Range = %q, want bytes 4-15/16", got)
	}
	rec = conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": "bytes=-4"})
	if got := rec.Header().Get("Content-Range"); got != "bytes 12-15/16" {
		t.Fatalf("Content-Range = %q, want bytes 12-15/16", got)
	}
	rec = conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": "bytes=10-100"})
	if got := rec.Header().Get("Content-Range"); got != "bytes 10-15/16" {
		t.Fatalf("Content-Range = %q, want bytes 10-15/16", got)
	}

	// Unsatisfiable or malformed ranges are uniform 416s.
	bad := []string{
		"items=0-1",                      // unknown unit
		"bytes=",                         // empty range set
		"bytes=abc",                      // no dash
		"bytes=1-2,4-5",                  // multiple ranges
		"bytes=16-",                      // start beyond the body
		"bytes=100-200",                  // start beyond the body
		"bytes=5-3",                      // end before start
		"bytes=-0",                       // zero-length suffix
		"bytes=-",                        // empty suffix
		"bytes=1-x",                      // non-numeric end
		"bytes=99999999999999999999999-", // saturated start beyond the body
	}
	for _, value := range bad {
		rec := conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": value})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("Range %q: status = %d, want 416", value, rec.Code)
		}
		assertErrorCode(t, rec, "range_not_satisfiable")
		if got := rec.Header().Get("Content-Range"); got != "bytes */16" {
			t.Fatalf("Range %q: Content-Range = %q, want bytes */16", value, got)
		}
	}

	// Any range on an empty body is unsatisfiable.
	emptyCID := uploadBytes(t, h, nil)
	for _, value := range []string{"bytes=0-", "bytes=-1", "bytes=0-0"} {
		rec := conditionalRequest(h, http.MethodGet, "/v1/objects/"+emptyCID, map[string]string{"Range": value})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("empty body Range %q: status = %d, want 416", value, rec.Code)
		}
		if got := rec.Header().Get("Content-Range"); got != "bytes */0" {
			t.Fatalf("empty body Range %q: Content-Range = %q, want bytes */0", value, got)
		}
	}
}

func TestObjectHead(t *testing.T) {
	h := Handler()
	payload := []byte("head me")
	cid := uploadBytes(t, h, payload)
	path := "/v1/objects/" + cid
	etag := `"` + cid + `"`

	// HEAD reports the full GET headers, ignores Range and writes no body.
	rec := conditionalRequest(h, http.MethodHead, path, map[string]string{"Range": "bytes=0-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("HEAD ETag = %q, want %q", got, etag)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("HEAD Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("HEAD Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", got, len(payload))
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD wrote %d body bytes", rec.Body.Len())
	}

	// If-None-Match still applies to HEAD.
	rec = conditionalRequest(h, http.MethodHead, path, map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("HEAD If-None-Match: status = %d body = %d bytes, want 304 empty", rec.Code, rec.Body.Len())
	}

	// Existence and CID checks still run for HEAD.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = conditionalRequest(h, http.MethodHead, "/v1/objects/"+missing, nil)
	if rec.Code != http.StatusNotFound || rec.Body.Len() != 0 {
		t.Fatalf("HEAD missing: status = %d body = %d bytes", rec.Code, rec.Body.Len())
	}
	rec = conditionalRequest(h, http.MethodHead, "/v1/objects/bogus", nil)
	if rec.Code != http.StatusBadRequest || rec.Body.Len() != 0 {
		t.Fatalf("HEAD invalid cid: status = %d body = %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestBlockGetConditionalRangeHead(t *testing.T) {
	h := Handler()
	payload := []byte("block level payload")
	uploadBytes(t, h, payload)
	blockCID := upload(t, h, payload).Chunks[0]
	path := "/v1/blocks/" + blockCID
	etag := `"` + blockCID + `"`

	rec := conditionalRequest(h, http.MethodGet, path, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != etag ||
		rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("GET block: status=%d etag=%q accept-ranges=%q",
			rec.Code, rec.Header().Get("ETag"), rec.Header().Get("Accept-Ranges"))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("block body mismatch")
	}

	rec = conditionalRequest(h, http.MethodGet, path, map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("block If-None-Match: status = %d, want 304 empty", rec.Code)
	}

	rec = conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": "bytes=6-11"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("block range: status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "level " {
		t.Fatalf("block range body = %q, want %q", got, "level ")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 6-11/19" {
		t.Fatalf("block Content-Range = %q, want bytes 6-11/19", got)
	}

	rec = conditionalRequest(h, http.MethodGet, path, map[string]string{"Range": "rows=0-1"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("block bad unit: status = %d, want 416", rec.Code)
	}
	assertErrorCode(t, rec, "range_not_satisfiable")
	if got := rec.Header().Get("Content-Range"); got != "bytes */19" {
		t.Fatalf("block 416 Content-Range = %q, want bytes */19", got)
	}

	rec = conditionalRequest(h, http.MethodHead, path, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 ||
		rec.Header().Get("Content-Length") != strconv.Itoa(len(payload)) ||
		rec.Header().Get("ETag") != etag {
		t.Fatalf("HEAD block: status=%d len=%q etag=%q body=%d",
			rec.Code, rec.Header().Get("Content-Length"), rec.Header().Get("ETag"), rec.Body.Len())
	}

	// Resource errors precede conditional evaluation.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = conditionalRequest(h, http.MethodHead, "/v1/blocks/"+missing, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD missing block: status = %d, want 404", rec.Code)
	}
}

func TestGatewayConditionalRangeHead(t *testing.T) {
	h := Handler()
	fileBody := []byte("gateway file contents")
	fileCID := uploadBytes(t, h, fileBody)
	dirBody := marshalDirectory(t, []dirEntrySpec{{name: "doc", typ: "file", cid: fileCID}})
	dirCID := uploadBytes(t, h, dirBody)
	filePath := "/v1/gateway/" + dirCID + "/doc"
	etag := `"` + fileCID + `"`

	// The gateway file entry carries the final object's ETag.
	rec := conditionalRequest(h, http.MethodGet, filePath, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != etag ||
		rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("gateway GET: status=%d etag=%q accept-ranges=%q",
			rec.Code, rec.Header().Get("ETag"), rec.Header().Get("Accept-Ranges"))
	}

	rec = conditionalRequest(h, http.MethodGet, filePath, map[string]string{"If-None-Match": "W/" + etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("gateway If-None-Match: status = %d, want 304 empty", rec.Code)
	}

	rec = conditionalRequest(h, http.MethodGet, filePath, map[string]string{"Range": "bytes=8-11"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "file" {
		t.Fatalf("gateway range: status = %d body = %q, want 206 %q", rec.Code, rec.Body.String(), "file")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 8-11/21" {
		t.Fatalf("gateway Content-Range = %q, want bytes 8-11/21", got)
	}

	rec = conditionalRequest(h, http.MethodGet, filePath, map[string]string{"Range": "bytes=99-"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("gateway unsatisfiable: status = %d, want 416", rec.Code)
	}
	assertErrorCode(t, rec, "range_not_satisfiable")

	// HEAD on a directory reports the directory media type and full length.
	rec = conditionalRequest(h, http.MethodHead, "/v1/gateway/"+dirCID, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("gateway HEAD directory: status = %d body = %d", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Type"); got != directoryMediaType {
		t.Fatalf("gateway HEAD Content-Type = %q, want %q", got, directoryMediaType)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(dirBody)) {
		t.Fatalf("gateway HEAD Content-Length = %q, want %d", got, len(dirBody))
	}
	if got := rec.Header().Get("ETag"); got != `"`+dirCID+`"` {
		t.Fatalf("gateway HEAD ETag = %q", got)
	}

	// Resolution errors precede conditional evaluation.
	rec = conditionalRequest(h, http.MethodGet, "/v1/gateway/"+dirCID+"/nope",
		map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("gateway missing path with If-None-Match: status = %d, want 404", rec.Code)
	}
	assertErrorCode(t, rec, "path_not_found")
}
