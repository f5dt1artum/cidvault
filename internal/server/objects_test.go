package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/f5dt1artum/cidvault/internal/store"
)

func apiError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error content-type = %q, want application/json", ct)
	}
	return payload.Error.Code
}

func upload(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/objects", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeObject(t *testing.T, rec *httptest.ResponseRecorder) objectResponse {
	t.Helper()
	var resp objectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("object response is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	return resp
}

func TestUploadCreateThenDuplicate(t *testing.T) {
	h := Handler()
	body := []byte("hello content-addressed world")

	first := upload(t, h, body, "application/octet-stream")
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d, want 201", first.Code)
	}
	obj := decodeObject(t, first)
	if !obj.Created || obj.Size != int64(len(body)) || obj.ChunkSize != store.ChunkSize {
		t.Fatalf("unexpected first response: %+v", obj)
	}
	if len(obj.Chunks) != 1 || !strings.HasPrefix(obj.CID, "sha256:") {
		t.Fatalf("unexpected cid/chunks: %+v", obj)
	}

	second := upload(t, h, body, "application/octet-stream")
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate upload status = %d, want 200", second.Code)
	}
	obj2 := decodeObject(t, second)
	if obj2.Created {
		t.Fatalf("duplicate upload must report created=false")
	}
	if obj2.CID != obj.CID || obj2.Size != obj.Size ||
		fmt.Sprint(obj2.Chunks) != fmt.Sprint(obj.Chunks) {
		t.Fatalf("duplicate response differs: %+v vs %+v", obj2, obj)
	}
}

func TestUploadEmptyBody(t *testing.T) {
	h := Handler()
	rec := upload(t, h, nil, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	obj := decodeObject(t, rec)
	if obj.Size != 0 || len(obj.Chunks) != 0 {
		t.Fatalf("empty body: size=%d chunks=%v, want 0 and []", obj.Size, obj.Chunks)
	}
	// Empty list must serialize as [], not null.
	if !strings.Contains(rec.Body.String(), `"chunks":[]`) {
		t.Fatalf("empty chunks must encode as []: %s", rec.Body.String())
	}
}

func TestRoundTripAndManifest(t *testing.T) {
	h := Handler()
	body := bytes.Repeat([]byte("chunk-boundary-pattern-"), 200000) // ~4.4 MiB, multi-chunk

	rec := upload(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	obj := decodeObject(t, rec)
	if len(obj.Chunks) != 5 {
		t.Fatalf("chunk count = %d, want 5", len(obj.Chunks))
	}
	if obj.Chunks[len(obj.Chunks)-1] == obj.Chunks[0] {
		// last chunk is a partial tail, so its hash must differ
		t.Fatalf("partial tail chunk unexpectedly identical to first chunk")
	}

	// Byte-for-byte retrieval.
	get := httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil)
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, get)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET object status = %d", grec.Code)
	}
	if ct := grec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("GET object content-type = %q", ct)
	}
	if !bytes.Equal(grec.Body.Bytes(), body) {
		t.Fatalf("retrieved bytes differ from uploaded body")
	}

	// Manifest.
	mreq := httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID+"/manifest", nil)
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, mreq)
	if mrec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", mrec.Code)
	}
	var man manifestResponse
	if err := json.Unmarshal(mrec.Body.Bytes(), &man); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if man.CID != obj.CID || man.Size != int64(len(body)) ||
		man.ChunkSize != store.ChunkSize || fmt.Sprint(man.Chunks) != fmt.Sprint(obj.Chunks) {
		t.Fatalf("manifest mismatch: %+v vs %+v", man, obj)
	}
}

func TestPayloadTooLargeLeavesNoObject(t *testing.T) {
	h := Handler()
	oversized := make([]byte, store.MaxObjectSize+1)

	rec := upload(t, h, oversized, "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if code := apiError(t, rec); code != "payload_too_large" {
		t.Fatalf("error code = %q, want payload_too_large", code)
	}

	// Exactly the limit is accepted.
	atLimit := make([]byte, store.MaxObjectSize)
	ok := upload(t, h, atLimit, "application/octet-stream")
	if ok.Code != http.StatusCreated {
		t.Fatalf("size-at-limit status = %d, want 201", ok.Code)
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"text/plain":       "text/plain",
		"application/json": "application/json",
		"garbage":          "!!!not-a-type",
		"missing":          "",
	}
	for name, ct := range cases {
		rec := upload(t, h, []byte("x"), ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: status = %d, want 415", name, rec.Code)
		}
		if code := apiError(t, rec); code != "unsupported_media_type" {
			t.Fatalf("%s: code = %q", name, code)
		}
	}

	// The type is matched case-insensitively and parameter-tolerantly, so
	// these are still application/octet-stream uploads.
	for name, ct := range map[string]string{
		"with_charset": "application/octet-stream; charset=utf-8",
		"case_variant": "Application/Octet-Stream",
	} {
		rec := upload(t, h, []byte(name), ct)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d, want 201", name, rec.Code)
		}
	}
}

func TestInvalidAndMissingCID(t *testing.T) {
	h := Handler()
	bad := []string{
		"/v1/objects/foo",
		"/v1/objects/sha256:" + strings.Repeat("A", 64), // uppercase
		"/v1/objects/sha256:" + strings.Repeat("0", 63), // too short
		"/v1/objects/sha256:" + strings.Repeat("0", 65), // too long
		"/v1/objects/sha256:" + strings.Repeat("g", 64), // non-hex
		"/v1/objects/sha256",
	}
	for _, path := range bad {
		for _, suffix := range []string{"", "/manifest"} {
			req := httptest.NewRequest(http.MethodGet, path+suffix, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s: status = %d, want 400", path+suffix, rec.Code)
			}
			if code := apiError(t, rec); code != "invalid_cid" {
				t.Fatalf("GET %s: code = %q", path+suffix, code)
			}
		}
	}

	// Well-formed but unknown.
	missing := "/v1/objects/sha256:" + strings.Repeat("0", 64)
	for _, path := range []string{missing, missing + "/manifest"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404", path, rec.Code)
		}
		if code := apiError(t, rec); code != "object_not_found" {
			t.Fatalf("GET %s: code = %q", path, code)
		}
	}
}

func TestMethodNotAllowedAllowHeader(t *testing.T) {
	h := Handler()
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodGet, "/v1/objects", http.MethodPost},
		{http.MethodPut, "/v1/objects", http.MethodPost},
		{http.MethodDelete, "/v1/objects", http.MethodPost},
		{http.MethodPost, "/v1/objects/sha256:" + strings.Repeat("a", 64), http.MethodGet},
		{http.MethodDelete, "/v1/objects/sha256:" + strings.Repeat("a", 64) + "/manifest", http.MethodGet},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
		}
		if code := apiError(t, rec); code != "method_not_allowed" {
			t.Fatalf("%s %s: code = %q", tc.method, tc.path, code)
		}
	}
}

func TestConcurrentUploadsOneCreatedAndNeverPartial(t *testing.T) {
	ts := httptest.NewServer(Handler())
	defer ts.Close()

	body := bytes.Repeat([]byte{'q'}, store.ChunkSize+7)
	const n = 24

	var wg sync.WaitGroup
	statuses := make([]int, n)
	cids := make([]string, n)
	creates := make(chan bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/objects", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Errorf("upload %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
			data, _ := io.ReadAll(resp.Body)
			var or objectResponse
			_ = json.Unmarshal(data, &or)
			cids[i] = or.CID
			creates <- or.Created
		}(i)
	}
	close(start)
	wg.Wait()
	close(creates)

	createdCount := 0
	for c := range creates {
		if c {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=true in %d/%d responses, want exactly 1", createdCount, n)
	}
	creators := 0
	for i := 0; i < n; i++ {
		if cids[i] != cids[0] {
			t.Fatalf("response %d cid=%q differs from %q", i, cids[i], cids[0])
		}
		switch statuses[i] {
		case http.StatusCreated:
			creators++
		case http.StatusOK:
		default:
			t.Fatalf("response %d: unexpected status %d", i, statuses[i])
		}
	}
	if creators != 1 {
		t.Fatalf("status 201 returned to %d/%d clients, want exactly 1", creators, n)
	}

	// Final retrieval is the complete object for every reader.
	resp, err := http.Get(ts.URL + "/v1/objects/" + cids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(data, body) {
		t.Fatalf("final GET: status=%d match=%v", resp.StatusCode, bytes.Equal(data, body))
	}
}

func TestHealthzUnchanged(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d", rec.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health payload not JSON: %v", err)
	}
	if payload["status"] != "ok" || payload["service"] != "cidvault" || payload["version"] != Version {
		t.Fatalf("health payload changed: %v", payload)
	}

	// Baseline behavior for non-GET stays 405.
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if head.Code != http.StatusMethodNotAllowed || head.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD /healthz: status=%d allow=%q, want 405 GET", head.Code, head.Header().Get("Allow"))
	}
}
