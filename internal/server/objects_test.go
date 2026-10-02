package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func postBody(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/objects", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeObjectResponse(t *testing.T, rec *httptest.ResponseRecorder) objectResponse {
	t.Helper()
	var resp objectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return resp
}

// expectedRootCID recomputes the root identifier from the documented
// deterministic manifest encoding.
func expectedRootCID(size int, cids []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "size:%d\n", size)
	for _, c := range cids {
		b.WriteString(c)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestUploadAndRetrieveRoundTrip(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	rec := postBody(t, h, payload, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.Size != len(payload) || resp.ChunkSize != ChunkSize || !resp.Created {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(resp.Chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(resp.Chunks))
	}
	for i, cid := range resp.Chunks {
		chunk := payload[i*ChunkSize:]
		if len(chunk) > ChunkSize {
			chunk = chunk[:ChunkSize]
		}
		sum := sha256.Sum256(chunk)
		if want := "sha256:" + hex.EncodeToString(sum[:]); cid != want {
			t.Fatalf("chunk %d cid = %q, want %q", i, cid, want)
		}
	}
	if want := expectedRootCID(len(payload), resp.Chunks); resp.CID != want {
		t.Fatalf("root cid = %q, want %q", resp.CID, want)
	}

	// Byte-identical retrieval.
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+resp.CID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getRec.Code, http.StatusOK)
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("GET Content-Type = %q, want application/octet-stream", ct)
	}
	if !bytes.Equal(getRec.Body.Bytes(), payload) {
		t.Fatal("retrieved bytes differ from uploaded payload")
	}

	// Manifest retrieval.
	manRec := httptest.NewRecorder()
	h.ServeHTTP(manRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+resp.CID+"/manifest", nil))
	if manRec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want %d", manRec.Code, http.StatusOK)
	}
	var man manifestResponse
	if err := json.Unmarshal(manRec.Body.Bytes(), &man); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if man.CID != resp.CID || man.Size != resp.Size || man.ChunkSize != ChunkSize {
		t.Fatalf("unexpected manifest: %+v", man)
	}
	if strings.Join(man.Chunks, ",") != strings.Join(resp.Chunks, ",") {
		t.Fatalf("manifest chunks differ: %+v", man.Chunks)
	}
}

func TestDuplicateUploadReturnsSameCID(t *testing.T) {
	h := Handler()
	payload := []byte("hello content addressing")

	first := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
	if !first.Created {
		t.Fatal("first upload should report created=true")
	}
	rec := postBody(t, h, payload, "application/octet-stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want %d", rec.Code, http.StatusOK)
	}
	second := decodeObjectResponse(t, rec)
	if second.Created {
		t.Fatal("duplicate upload should report created=false")
	}
	if second.CID != first.CID {
		t.Fatalf("duplicate cid = %q, want %q", second.CID, first.CID)
	}
}

func TestDistinctContentYieldsDistinctCID(t *testing.T) {
	h := Handler()
	a := decodeObjectResponse(t, postBody(t, h, []byte("aaa"), "application/octet-stream"))
	b := decodeObjectResponse(t, postBody(t, h, []byte("aab"), "application/octet-stream"))
	if a.CID == b.CID {
		t.Fatal("different content produced the same root cid")
	}
}

func TestEmptyBodyHasNoChunks(t *testing.T) {
	h := Handler()
	rec := postBody(t, h, nil, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.Size != 0 || len(resp.Chunks) != 0 {
		t.Fatalf("unexpected empty-object response: %+v", resp)
	}
	if !strings.Contains(rec.Body.String(), `"chunks":[]`) {
		t.Fatalf("chunks should encode as [], got %s", rec.Body.String())
	}
	if want := expectedRootCID(0, nil); resp.CID != want {
		t.Fatalf("empty root cid = %q, want %q", resp.CID, want)
	}
}

func TestOversizedUploadRejected(t *testing.T) {
	h := Handler()
	body := make([]byte, MaxObjectSize+1)
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	assertErrorCode(t, rec, "payload_too_large")

	// The oversized attempt must not leave a partial object behind.
	manRec := httptest.NewRecorder()
	h.ServeHTTP(manRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+expectedRootCID(0, nil)+"/manifest", nil))
	if manRec.Code != http.StatusNotFound {
		t.Fatalf("store should be empty after rejected upload, got %d", manRec.Code)
	}
}

func TestExactMaxSizeAccepted(t *testing.T) {
	h := Handler()
	body := make([]byte, MaxObjectSize)
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeObjectResponse(t, rec)
	if len(resp.Chunks) != MaxObjectSize/ChunkSize {
		t.Fatalf("chunks = %d, want %d", len(resp.Chunks), MaxObjectSize/ChunkSize)
	}
}

func TestWrongMediaTypeRejected(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"text/plain", "application/json", ""} {
		rec := postBody(t, h, []byte("data"), ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}
}

func TestInvalidAndUnknownCID(t *testing.T) {
	h := Handler()
	for _, path := range []string{
		"/v1/objects/not-a-cid",
		"/v1/objects/sha256:ABC",
		"/v1/objects/sha256:" + strings.Repeat("a", 63),
		"/v1/objects/sha256:" + strings.Repeat("g", 64),
	} {
		for _, suffix := range []string{"", "/manifest"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+suffix, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s%s: status = %d, want %d", path, suffix, rec.Code, http.StatusBadRequest)
			}
			assertErrorCode(t, rec, "invalid_cid")
		}
	}

	missing := "sha256:" + strings.Repeat("0", 64)
	for _, suffix := range []string{"", "/manifest"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+missing+suffix, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET missing%s: status = %d, want %d", suffix, rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "object_not_found")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := Handler()
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodGet, "/v1/objects", http.MethodPost},
		{http.MethodPut, "/v1/objects", http.MethodPost},
		{http.MethodPost, "/v1/objects/" + strings.Repeat("0", 64), http.MethodGet},
		{http.MethodDelete, "/v1/objects/" + strings.Repeat("0", 64) + "/manifest", http.MethodGet},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, allow, tc.allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestConcurrentIdenticalUploadsCreateOnce(t *testing.T) {
	h := Handler()
	payload := bytes.Repeat([]byte("concurrent"), 1000)

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	created := make([]bool, n)
	cids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postBody(t, h, payload, "application/octet-stream")
			resp := decodeObjectResponse(t, rec)
			codes[i], created[i], cids[i] = rec.Code, resp.Created, resp.CID
		}(i)
	}
	wg.Wait()

	trues := 0
	for i := 0; i < n; i++ {
		if created[i] {
			trues++
			if codes[i] != http.StatusCreated {
				t.Fatalf("created upload returned status %d, want %d", codes[i], http.StatusCreated)
			}
		} else if codes[i] != http.StatusOK {
			t.Fatalf("duplicate upload returned status %d, want %d", codes[i], http.StatusOK)
		}
		if cids[i] != cids[0] {
			t.Fatal("concurrent uploads returned different cids")
		}
	}
	if trues != 1 {
		t.Fatalf("created=true count = %d, want 1", trues)
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %q, want %q", body.Error.Code, want)
	}
}
