package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func getBlock(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/blocks/"+cid, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getStats(t *testing.T, h http.Handler) statsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return resp
}

func mustGC(t *testing.T, h http.Handler, query string) {
	t.Helper()
	if code, _ := runGC(t, h, query); code != http.StatusOK {
		t.Fatalf("gc status = %d, want %d", code, http.StatusOK)
	}
}

func TestBlockRoundTrip(t *testing.T) {
	h := Handler()
	payload := []byte(strings.Repeat("cidvault-block-", 1000))
	resp := upload(t, h, payload)
	if len(resp.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(resp.Chunks))
	}

	rec := getBlock(t, h, resp.Chunks[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("block bytes do not match uploaded chunk")
	}
}

func TestBlockReadErrors(t *testing.T) {
	h := Handler()
	upload(t, h, []byte("hello"))

	rec := getBlock(t, h, "not-a-cid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_cid")

	// Well-formed but unreferenced identifier.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = getBlock(t, h, missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "block_not_found")
}

func TestNewEndpointsRejectNonGet(t *testing.T) {
	h := Handler()
	resp := upload(t, h, []byte("hello"))
	cases := []struct {
		path  string
		allow string
	}{
		{"/v1/blocks/" + resp.Chunks[0], "GET, HEAD"},
		{"/v1/storage/stats", http.MethodGet},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: status = %d, want %d", method, tc.path, rec.Code, http.StatusMethodNotAllowed)
			}
			if allow := rec.Header().Get("Allow"); allow != tc.allow {
				t.Fatalf("%s %s: Allow = %q, want %q", method, tc.path, allow, tc.allow)
			}
			assertErrorCode(t, rec, "method_not_allowed")
		}
	}
}

func TestStorageStatsEmptyStore(t *testing.T) {
	h := Handler()
	st := getStats(t, h)
	if st != (statsResponse{}) {
		t.Fatalf("stats = %+v, want all zero", st)
	}
}

func TestStorageStatsCounting(t *testing.T) {
	h := Handler()

	// Empty object counts only toward objects.
	upload(t, h, nil)
	st := getStats(t, h)
	if st.Objects != 1 || st.LogicalBytes != 0 || st.Blocks != 0 || st.StoredBytes != 0 {
		t.Fatalf("stats after empty object = %+v", st)
	}

	// One object whose body repeats the same chunk-sized prefix: the
	// repeated block is stored and counted once.
	chunk := bytes.Repeat([]byte("a"), ChunkSize)
	body := append(append([]byte{}, chunk...), chunk...)
	resp := upload(t, h, body)
	if len(resp.Chunks) != 2 || resp.Chunks[0] != resp.Chunks[1] {
		t.Fatalf("expected two identical chunks, got %v", resp.Chunks)
	}
	st = getStats(t, h)
	want := statsResponse{Objects: 2, LogicalBytes: 2 * ChunkSize, Blocks: 1, StoredBytes: ChunkSize}
	if st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}

	// A second object sharing the same block adds logical bytes only.
	other := append(append([]byte{}, chunk...), []byte("tail")...)
	resp2 := upload(t, h, other)
	st = getStats(t, h)
	want = statsResponse{
		Objects:      3,
		LogicalBytes: 2*ChunkSize + ChunkSize + 4,
		Blocks:       2,
		StoredBytes:  ChunkSize + 4,
	}
	if st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}

	// Re-uploading an existing object changes nothing.
	upload(t, h, body)
	upload(t, h, other)
	if st = getStats(t, h); st != want {
		t.Fatalf("stats after duplicate uploads = %+v, want %+v", st, want)
	}

	// Both shared blocks stay readable.
	for _, cid := range append(resp.Chunks[:1], resp2.Chunks[1]) {
		if rec := getBlock(t, h, cid); rec.Code != http.StatusOK {
			t.Fatalf("block %s: status = %d, want %d", cid, rec.Code, http.StatusOK)
		}
	}
}

func TestStorageStatsAcrossGC(t *testing.T) {
	h := Handler()
	shared := bytes.Repeat([]byte("s"), ChunkSize)
	pinned := upload(t, h, append(append([]byte{}, shared...), []byte("kept")...))
	doomed := upload(t, h, append(append([]byte{}, shared...), []byte("gone")...))

	// Pin the first object so only the second is collected.
	rec := putPin(t, h, pinned.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d, want %d", rec.Code, http.StatusCreated)
	}

	// Dry run changes neither objects, blocks, nor stats.
	mustGC(t, h, "?dryRun=true")
	before := statsResponse{Objects: 2, LogicalBytes: 2 * (ChunkSize + 4), Blocks: 3, StoredBytes: ChunkSize + 8}
	if st := getStats(t, h); st != before {
		t.Fatalf("stats after dry run = %+v, want %+v", st, before)
	}
	for _, cid := range doomed.Chunks {
		if rec := getBlock(t, h, cid); rec.Code != http.StatusOK {
			t.Fatalf("block %s after dry run: status = %d, want %d", cid, rec.Code, http.StatusOK)
		}
	}

	mustGC(t, h, "")
	after := statsResponse{Objects: 1, LogicalBytes: ChunkSize + 4, Blocks: 2, StoredBytes: ChunkSize + 4}
	if st := getStats(t, h); st != after {
		t.Fatalf("stats after gc = %+v, want %+v", st, after)
	}

	// The shared block remains readable; the exclusive block is gone.
	if rec := getBlock(t, h, doomed.Chunks[0]); rec.Code != http.StatusOK {
		t.Fatalf("shared block: status = %d, want %d", rec.Code, http.StatusOK)
	}
	rec = getBlock(t, h, doomed.Chunks[1])
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unreferenced block: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "block_not_found")
}

func TestConcurrentSharedUploadsCountedOnce(t *testing.T) {
	h := Handler()
	body := []byte(strings.Repeat("shared-", 1000))

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			upload(t, h, body)
		}()
	}
	wg.Wait()

	want := statsResponse{Objects: 1, LogicalBytes: len(body), Blocks: 1, StoredBytes: len(body)}
	if st := getStats(t, h); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
}

// TestStatsShapeUsesDocumentedKeys guards the fixed JSON key set.
func TestStatsShapeUsesDocumentedKeys(t *testing.T) {
	h := Handler()
	upload(t, h, []byte("x"))
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var payload map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	want := map[string]int{"objects": 1, "logicalBytes": 1, "blocks": 1, "storedBytes": 1}
	if fmt.Sprintf("%v", payload) != fmt.Sprintf("%v", want) {
		t.Fatalf("stats payload = %v, want %v", payload, want)
	}
}
