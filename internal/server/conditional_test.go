package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// readRequest issues a GET or HEAD against a read endpoint with optional
// headers and returns the recorder.
func readRequest(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReadSuccessHeaders(t *testing.T) {
	h := Handler()
	payload := []byte("conditional-read-payload")
	resp := upload(t, h, payload)

	rec := readRequest(h, http.MethodGet, "/v1/objects/"+resp.CID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("object status = %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"`+resp.CID+`"` {
		t.Fatalf("object ETag = %q, want quoted cid", etag)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("object Accept-Ranges = %q, want bytes", ar)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("object Content-Type = %q", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(payload)) {
		t.Fatalf("object Content-Length = %q, want %d", cl, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("object body changed")
	}

	rec = readRequest(h, http.MethodGet, "/v1/blocks/"+resp.Chunks[0], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("block status = %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"`+resp.Chunks[0]+`"` {
		t.Fatalf("block ETag = %q, want quoted block cid", etag)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("block Accept-Ranges = %q, want bytes", ar)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("block body changed")
	}
}

func TestGatewayReadSuccessHeaders(t *testing.T) {
	h := Handler()
	rootCID, _, fileCID := buildTree(t, h)

	// A file target carries the file object's ETag.
	rec := readRequest(h, http.MethodGet, "/v1/gateway/"+rootCID+"/top.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("file status = %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"`+fileCID+`"` {
		t.Fatalf("file ETag = %q, want quoted file cid %q", etag, fileCID)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("file Accept-Ranges = %q, want bytes", ar)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("file Content-Type = %q", ct)
	}
	if rec.Body.String() != "file-bytes" {
		t.Fatalf("file body = %q", rec.Body.String())
	}

	// A directory target carries the directory object's ETag.
	rec = readRequest(h, http.MethodGet, "/v1/gateway/"+rootCID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("directory status = %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"`+rootCID+`"` {
		t.Fatalf("directory ETag = %q, want quoted root cid %q", etag, rootCID)
	}
	if ct := rec.Header().Get("Content-Type"); ct != directoryMediaType {
		t.Fatalf("directory Content-Type = %q", ct)
	}
}

func TestIfNoneMatch(t *testing.T) {
	h := Handler()
	resp := upload(t, h, []byte("etag-target"))
	etag := `"` + resp.CID + `"`
	path := "/v1/objects/" + resp.CID

	for _, value := range []string{
		"*",
		etag,
		`W/` + etag,
		`"sha256:` + strings.Repeat("0", 64) + `", ` + etag,
		etag + `, "other"`,
	} {
		rec := readRequest(h, http.MethodGet, path, map[string]string{"If-None-Match": value})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match %q: status = %d, want 304", value, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q: 304 carried a body", value)
		}
	}

	// Non-matching tags fall through to a normal 200.
	rec := readRequest(h, http.MethodGet, path, map[string]string{
		"If-None-Match": `"sha256:` + strings.Repeat("0", 64) + `"`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("non-matching If-None-Match: status = %d, want 200", rec.Code)
	}

	// Unparseable values are a 400, ahead of any Range handling.
	for _, value := range []string{"not-a-tag", `"unterminated`, `*, "x"`, `"a" "b"`} {
		rec := readRequest(h, http.MethodGet, path, map[string]string{
			"If-None-Match": value,
			"Range":         "bytes=0-1",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("If-None-Match %q: status = %d, want 400", value, rec.Code)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// A match wins over Range, even an unsatisfiable one.
	rec = readRequest(h, http.MethodGet, path, map[string]string{
		"If-None-Match": etag,
		"Range":         "bytes=999-1000",
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("match plus bad range: status = %d, want 304", rec.Code)
	}

	// Resource errors win over If-None-Match.
	rec = readRequest(h, http.MethodGet, "/v1/objects/sha256:"+strings.Repeat("0", 64),
		map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object with If-None-Match: status = %d, want 404", rec.Code)
	}
	assertErrorCode(t, rec, "object_not_found")
	rec = readRequest(h, http.MethodGet, "/v1/objects/not-a-cid",
		map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cid with If-None-Match: status = %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_cid")
}

func TestRangeSatisfiable(t *testing.T) {
	h := Handler()
	// Cross the chunk boundary so range assembly walks multiple chunks.
	payload := bytes.Repeat([]byte("0123456789abcdef"), (ChunkSize/16)+10)
	resp := upload(t, h, payload)
	size := len(payload)

	cases := []struct {
		header    string
		wantStart int
		wantEnd   int // inclusive
	}{
		{"bytes=0-99", 0, 99},
		{"bytes=100-", 100, size - 1},
		{"bytes=-100", size - 100, size - 1},
		{"bytes=0-" + strconv.Itoa(size-1), 0, size - 1},   // exact full body
		{"bytes=0-" + strconv.Itoa(size+100), 0, size - 1}, // end clipped
		{"bytes=-" + strconv.Itoa(size+100), 0, size - 1},  // suffix clipped
		{"bytes=" + strconv.Itoa(size-1) + "-", size - 1, size - 1},
		{"bytes=" + strconv.Itoa(ChunkSize-5) + "-" + strconv.Itoa(ChunkSize+5), ChunkSize - 5, ChunkSize + 5},
	}
	for _, tc := range cases {
		rec := readRequest(h, http.MethodGet, "/v1/objects/"+resp.CID, map[string]string{"Range": tc.header})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("%s: status = %d, want 206", tc.header, rec.Code)
		}
		wantLen := tc.wantEnd - tc.wantStart + 1
		if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes %d-%d/%d", tc.wantStart, tc.wantEnd, size) {
			t.Fatalf("%s: Content-Range = %q", tc.header, cr)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(wantLen) {
			t.Fatalf("%s: Content-Length = %q, want %d", tc.header, cl, wantLen)
		}
		if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
			t.Fatalf("%s: Accept-Ranges = %q", tc.header, ar)
		}
		if etag := rec.Header().Get("ETag"); etag != `"`+resp.CID+`"` {
			t.Fatalf("%s: ETag = %q", tc.header, etag)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload[tc.wantStart:tc.wantEnd+1]) {
			t.Fatalf("%s: body bytes do not match the selected interval", tc.header)
		}
	}

	// Blocks support the same single-range reads.
	rec := readRequest(h, http.MethodGet, "/v1/blocks/"+resp.Chunks[0], map[string]string{"Range": "bytes=1-3"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("block range: status = %d, want 206", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 1-3/%d", ChunkSize) {
		t.Fatalf("block range: Content-Range = %q", cr)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[1:4]) {
		t.Fatalf("block range: wrong bytes")
	}

	// And the gateway, for both files and directories.
	rootCID, _, _ := buildTree(t, h)
	rec = readRequest(h, http.MethodGet, "/v1/gateway/"+rootCID+"/top.txt", map[string]string{"Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "file" {
		t.Fatalf("gateway file range: status = %d body = %q", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-3/10" {
		t.Fatalf("gateway file range: Content-Range = %q", cr)
	}
}

func TestRangeNotSatisfiable(t *testing.T) {
	h := Handler()
	payload := []byte("range-me")
	resp := upload(t, h, payload)
	size := len(payload)

	bad := []string{
		"items=0-1",     // unit other than bytes
		"bytes=",        // empty spec
		"bytes=-",       // no numbers at all
		"bytes=abc",     // no dash
		"bytes=a-b",     // non-numeric
		"bytes=1-2-3",   // extra dash
		"bytes=0-1,2-3", // multiple ranges
		"bytes=8-",      // start beyond the body
		"bytes=8-9",     // start beyond the body, closed
		"bytes=5-3",     // end before start
		"bytes=-0",      // zero-length suffix
		"bytes= 0-1",    // inner whitespace
	}
	for _, header := range bad {
		rec := readRequest(h, http.MethodGet, "/v1/objects/"+resp.CID, map[string]string{"Range": header})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("%s: status = %d, want 416", header, rec.Code)
		}
		if cr := rec.Header().Get("Content-Range"); cr != "bytes */"+strconv.Itoa(size) {
			t.Fatalf("%s: Content-Range = %q, want bytes */%d", header, cr, size)
		}
		assertErrorCode(t, rec, "range_not_satisfiable")
	}

	// Any range on an empty body is unsatisfiable.
	empty := upload(t, h, nil)
	for _, header := range []string{"bytes=0-", "bytes=0-0", "bytes=-1"} {
		rec := readRequest(h, http.MethodGet, "/v1/objects/"+empty.CID, map[string]string{"Range": header})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("empty body %s: status = %d, want 416", header, rec.Code)
		}
		if cr := rec.Header().Get("Content-Range"); cr != "bytes */0" {
			t.Fatalf("empty body %s: Content-Range = %q, want bytes */0", header, cr)
		}
	}

	// Resource errors still win over Range.
	rec := readRequest(h, http.MethodGet, "/v1/blocks/sha256:"+strings.Repeat("0", 64),
		map[string]string{"Range": "not-a-range"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing block with range: status = %d, want 404", rec.Code)
	}
	assertErrorCode(t, rec, "block_not_found")
}

func TestHeadReads(t *testing.T) {
	h := Handler()
	payload := []byte("head-payload")
	resp := upload(t, h, payload)
	rootCID, _, fileCID := buildTree(t, h)

	cases := []struct {
		path  string
		cid   string
		media string
		size  int
	}{
		{"/v1/objects/" + resp.CID, resp.CID, "application/octet-stream", len(payload)},
		{"/v1/blocks/" + resp.Chunks[0], resp.Chunks[0], "application/octet-stream", len(payload)},
		{"/v1/gateway/" + rootCID + "/top.txt", fileCID, "application/octet-stream", 10},
	}
	for _, tc := range cases {
		rec := readRequest(h, http.MethodHead, tc.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("HEAD %s: status = %d, want 200", tc.path, rec.Code)
		}
		if etag := rec.Header().Get("ETag"); etag != `"`+tc.cid+`"` {
			t.Fatalf("HEAD %s: ETag = %q", tc.path, etag)
		}
		if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
			t.Fatalf("HEAD %s: Accept-Ranges = %q", tc.path, ar)
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.media {
			t.Fatalf("HEAD %s: Content-Type = %q, want %q", tc.path, ct, tc.media)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(tc.size) {
			t.Fatalf("HEAD %s: Content-Length = %q, want %d", tc.path, cl, tc.size)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD %s: body = %q, want empty", tc.path, rec.Body.String())
		}
	}

	// A directory HEAD mirrors its GET: same ETag, media type and length.
	get := readRequest(h, http.MethodGet, "/v1/gateway/"+rootCID, nil)
	head := readRequest(h, http.MethodHead, "/v1/gateway/"+rootCID, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD directory: status = %d body = %q", head.Code, head.Body.String())
	}
	if head.Header().Get("ETag") != `"`+rootCID+`"` ||
		head.Header().Get("Content-Type") != directoryMediaType ||
		head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatalf("HEAD directory headers = %v, want mirror of GET", head.Header())
	}

	// HEAD ignores Range entirely, even an unsatisfiable one.
	rec := readRequest(h, http.MethodHead, "/v1/objects/"+resp.CID, map[string]string{"Range": "bytes=999-1000"})
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD with range: status = %d, want 200", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(payload)) {
		t.Fatalf("HEAD with range: Content-Length = %q, want %d", cl, len(payload))
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD with range: unexpected body")
	}

	// HEAD still honors If-None-Match.
	rec = readRequest(h, http.MethodHead, "/v1/objects/"+resp.CID, map[string]string{"If-None-Match": `"` + resp.CID + `"`})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("HEAD with matching If-None-Match: status = %d body = %q", rec.Code, rec.Body.String())
	}

	// HEAD errors share the GET validation but never carry a body.
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/v1/objects/not-a-cid", http.StatusBadRequest},
		{"/v1/objects/sha256:" + strings.Repeat("0", 64), http.StatusNotFound},
		{"/v1/blocks/sha256:" + strings.Repeat("0", 64), http.StatusNotFound},
		{"/v1/gateway/" + rootCID + "/missing", http.StatusNotFound},
		{"/v1/gateway/not-a-cid", http.StatusBadRequest},
	} {
		rec := readRequest(h, http.MethodHead, tc.path, nil)
		if rec.Code != tc.status {
			t.Fatalf("HEAD %s: status = %d, want %d", tc.path, rec.Code, tc.status)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("HEAD %s: error carried a body", tc.path)
		}
	}
}

func TestReadEndpointsRejectOtherMethods(t *testing.T) {
	h := Handler()
	resp := upload(t, h, []byte("methods"))
	rootCID, _, _ := buildTree(t, h)
	paths := []string{
		"/v1/objects/" + resp.CID,
		"/v1/blocks/" + resp.Chunks[0],
		"/v1/gateway/" + rootCID,
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			rec := readRequest(h, method, path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: status = %d, want 405", method, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Fatalf("%s %s: Allow = %q, want GET, HEAD", method, path, allow)
			}
			assertErrorCode(t, rec, "method_not_allowed")
		}
	}
}
