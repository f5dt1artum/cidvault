package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func getRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func decodeStats(t *testing.T, rec *httptest.ResponseRecorder) storageStatsResponse {
	t.Helper()
	var resp storageStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return resp
}

// repeatedBlock builds one ChunkSize block filled with b so that hand-built
// objects share or repeat blocks deterministically.
func repeatedBlock(b byte) []byte {
	return bytes.Repeat([]byte{b}, ChunkSize)
}

func TestStorageStatsEmpty(t *testing.T) {
	h := Handler()
	rec := getRequest(t, h, http.MethodGet, "/v1/storage/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"objects":0,"logicalBytes":0,"blocks":0,"storedBytes":0}`+"\n" {
		t.Fatalf("empty stats = %q", got)
	}
}

func TestBlockReadReturnsRawBytes(t *testing.T) {
	h := Handler()
	x, y := repeatedBlock('a'), repeatedBlock('b')
	body := append(append([]byte{}, x...), y...)
	up := postBody(t, h, body, "application/octet-stream")
	resp := decodeObjectResponse(t, up)
	if len(resp.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(resp.Chunks))
	}

	rec := getRequest(t, h, http.MethodGet, "/v1/blocks/"+resp.Chunks[1])
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())

	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", ChunkSize) {
		t.Fatalf("Content-Length = %q, want %d", cl, ChunkSize)
	}
	if !bytes.Equal(rec.Body.Bytes(), y) {
		t.Fatal("block bytes do not match the original chunk")
	}
}

func TestBlockReadErrors(t *testing.T) {
	h := Handler()

	rec := getRequest(t, h, http.MethodGet, "/v1/blocks/sha256:zzzz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cid status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if code := decodeErrorCode(t, rec); code != "invalid_cid" {
		t.Fatalf("code = %q, want invalid_cid", code)
	}

	missing := chunkCID(repeatedBlock('z'))
	rec = getRequest(t, h, http.MethodGet, "/v1/blocks/"+missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing block status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if code := decodeErrorCode(t, rec); code != "block_not_found" {
		t.Fatalf("code = %q, want block_not_found", code)
	}
}

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	return payload.Error.Code
}

func TestBlocksAndStatsMethodNotAllowed(t *testing.T) {
	h := Handler()

	rec := getRequest(t, h, http.MethodPost, "/v1/blocks/"+chunkCID(repeatedBlock('q')))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("block POST status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("block Allow = %q, want GET", allow)
	}
	if code := decodeErrorCode(t, rec); code != "method_not_allowed" {
		t.Fatalf("code = %q, want method_not_allowed", code)
	}

	rec = getRequest(t, h, http.MethodDelete, "/v1/storage/stats")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stats DELETE status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("stats Allow = %q, want GET", allow)
	}
}

func TestStatsDeduplicateRepeatedAndSharedBlocks(t *testing.T) {
	h := Handler()
	x, y := repeatedBlock('a'), repeatedBlock('b')

	// Object 1: [X, Y] — two distinct blocks.
	postBody(t, h, append(append([]byte{}, x...), y...), "application/octet-stream")
	// Object 2: [X, X] — one distinct block, repeated inside the object.
	postBody(t, h, append(append([]byte{}, x...), x...), "application/octet-stream")
	// Object 3: empty body — counts as an object, contributes nothing else.
	postBody(t, h, []byte{}, "application/octet-stream")

	rec := getRequest(t, h, http.MethodGet, "/v1/storage/stats")
	got := decodeStats(t, rec)
	want := storageStatsResponse{
		Objects:      3,
		LogicalBytes: 4 * ChunkSize,
		Blocks:       2,
		StoredBytes:  2 * ChunkSize,
	}
	if got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}

	// Re-uploading the same object must not change anything.
	postBody(t, h, append(append([]byte{}, x...), y...), "application/octet-stream")
	rec = getRequest(t, h, http.MethodGet, "/v1/storage/stats")
	if got = decodeStats(t, rec); got != want {
		t.Fatalf("stats after duplicate upload = %+v, want %+v", got, want)
	}
}

func TestGarbageCollectionPrunesBlocksAndStatsAtomically(t *testing.T) {
	h := Handler()
	x, y := repeatedBlock('a'), repeatedBlock('b')

	xy := append(append([]byte{}, x...), y...)
	xx := append(append([]byte{}, x...), x...)
	xyResp := decodeObjectResponse(t, postBody(t, h, xy, "application/octet-stream"))
	xxResp := decodeObjectResponse(t, postBody(t, h, xx, "application/octet-stream"))
	postBody(t, h, []byte{}, "application/octet-stream")

	xCID, yCID := xyResp.Chunks[0], xyResp.Chunks[1]

	// Dry run changes nothing.
	rec := getRequest(t, h, http.MethodPost, "/v1/gc?dryRun=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run status = %d", rec.Code)
	}
	if rec := getRequest(t, h, http.MethodGet, "/v1/blocks/"+yCID); rec.Code != http.StatusOK {
		t.Fatalf("shared block after dry run: status = %d, want 200", rec.Code)
	}
	stats := decodeStats(t, getRequest(t, h, http.MethodGet, "/v1/storage/stats"))
	if stats.Objects != 3 || stats.Blocks != 2 || stats.StoredBytes != 2*ChunkSize {
		t.Fatalf("stats changed by dry run: %+v", stats)
	}

	// Pin the [X, X] object; [X, Y] and the empty object are collectible.
	pinRec := httptest.NewRecorder()
	pinReq := httptest.NewRequest(http.MethodPut, "/v1/pins/"+xxResp.CID, bytes.NewReader([]byte("{}")))
	pinReq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(pinRec, pinReq)
	if pinRec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d, want 201", pinRec.Code)
	}

	rec = getRequest(t, h, http.MethodPost, "/v1/gc")
	if rec.Code != http.StatusOK {
		t.Fatalf("gc status = %d", rec.Code)
	}
	var gc gcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &gc); err != nil {
		t.Fatalf("gc response is not JSON: %v", err)
	}
	// bytes stays the sum of candidate object body lengths, not dedup bytes.
	if gc.Objects != 2 || gc.Bytes != 2*ChunkSize {
		t.Fatalf("gc result = %+v, want objects 2 bytes %d", gc, 2*ChunkSize)
	}

	// Y is no longer referenced: immediately unreadable and out of stats;
	// X is shared with the pinned object: still readable, counted once.
	if rec := getRequest(t, h, http.MethodGet, "/v1/blocks/"+yCID); rec.Code != http.StatusNotFound {
		t.Fatalf("unreferenced block status = %d, want 404", rec.Code)
	}
	if rec := getRequest(t, h, http.MethodGet, "/v1/blocks/"+xCID); rec.Code != http.StatusOK {
		t.Fatalf("shared block status = %d, want 200", rec.Code)
	}
	stats = decodeStats(t, getRequest(t, h, http.MethodGet, "/v1/storage/stats"))
	want := storageStatsResponse{Objects: 1, LogicalBytes: 2 * ChunkSize, Blocks: 1, StoredBytes: ChunkSize}
	if stats != want {
		t.Fatalf("stats after gc = %+v, want %+v", stats, want)
	}

	// Re-uploading [X, Y] restores the block and its counts.
	postBody(t, h, xy, "application/octet-stream")
	if rec := getRequest(t, h, http.MethodGet, "/v1/blocks/"+yCID); rec.Code != http.StatusOK {
		t.Fatalf("re-uploaded block status = %d, want 200", rec.Code)
	}
	stats = decodeStats(t, getRequest(t, h, http.MethodGet, "/v1/storage/stats"))
	want = storageStatsResponse{Objects: 2, LogicalBytes: 4 * ChunkSize, Blocks: 2, StoredBytes: 2 * ChunkSize}
	if stats != want {
		t.Fatalf("stats after re-upload = %+v, want %+v", stats, want)
	}
}

func TestConcurrentUploadsDoNotDoubleCount(t *testing.T) {
	h := Handler()
	x, y := repeatedBlock('a'), repeatedBlock('b')
	xy := append(append([]byte{}, x...), y...)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			body := append([]byte{}, xy...)
			postBody(t, h, body, "application/octet-stream")
		}()
	}
	close(start)
	wg.Wait()

	stats := decodeStats(t, getRequest(t, h, http.MethodGet, "/v1/storage/stats"))
	want := storageStatsResponse{Objects: 1, LogicalBytes: 2 * ChunkSize, Blocks: 2, StoredBytes: 2 * ChunkSize}
	if stats != want {
		t.Fatalf("stats after concurrent identical uploads = %+v, want %+v", stats, want)
	}
}
