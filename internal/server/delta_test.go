package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// exportDelta posts a delta export request for cid with the given raw body
// and content type.
func exportDelta(t *testing.T, h http.Handler, cid, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/delta-bundles/"+cid, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// importDelta posts a delta import request with the given raw body and
// content type.
func importDelta(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/delta-bundles", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// haveBody builds the export request body listing the given identifiers.
func haveBody(t *testing.T, cids []string) string {
	t.Helper()
	raw, err := json.Marshal(map[string][]string{"have": cids})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// decodeBundle decodes an exported bundle or delta bundle document.
func decodeBundle(t *testing.T, rec *httptest.ResponseRecorder) bundleDocument {
	t.Helper()
	var doc bundleDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("bundle document is not JSON: %v", err)
	}
	return doc
}

func TestDeltaExportFiltersBlocks(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := upload(t, h, payload)

	rec := exportDelta(t, h, up.CID, `{"have":[]}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != deltaBundleMediaType {
		t.Fatalf("export Content-Type = %q, want %q", ct, deltaBundleMediaType)
	}
	doc := decodeBundle(t, rec)
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if doc.Root.CID != up.CID || doc.Root.Size != len(payload) || doc.Root.ChunkSize != ChunkSize {
		t.Fatalf("root = %+v, want cid=%s size=%d chunkSize=%d", doc.Root, up.CID, len(payload), ChunkSize)
	}
	if len(doc.Root.Chunks) != len(up.Chunks) {
		t.Fatalf("root chunks = %d, want %d", len(doc.Root.Chunks), len(up.Chunks))
	}
	for i, c := range doc.Root.Chunks {
		if c != up.Chunks[i] {
			t.Fatalf("root chunk %d = %s, want %s", i, c, up.Chunks[i])
		}
	}
	if len(doc.Blocks) != len(up.Chunks) {
		t.Fatalf("blocks = %d, want %d", len(doc.Blocks), len(up.Chunks))
	}
	got := []string{}
	for _, b := range doc.Blocks {
		got = append(got, b.CID)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("blocks not sorted by cid: %v", got)
	}
	want := append([]string{}, up.Chunks...)
	sort.Strings(want)
	for i, c := range got {
		if c != want[i] {
			t.Fatalf("block %d = %s, want %s", i, c, want[i])
		}
	}

	// Holding one block removes exactly it from the delta.
	rec = exportDelta(t, h, up.CID, haveBody(t, up.Chunks[:1]), "application/json")
	doc = decodeBundle(t, rec)
	if len(doc.Blocks) != len(up.Chunks)-1 {
		t.Fatalf("delta blocks = %d, want %d", len(doc.Blocks), len(up.Chunks)-1)
	}
	for _, b := range doc.Blocks {
		if b.CID == up.Chunks[0] {
			t.Fatalf("delta still carries held block %s", b.CID)
		}
	}

	// Holding everything yields an empty block list; unknown entries are
	// ignored.
	all := append(append([]string{}, up.Chunks...), chunkCID([]byte("unrelated")))
	rec = exportDelta(t, h, up.CID, haveBody(t, all), "application/json")
	doc = decodeBundle(t, rec)
	if len(doc.Blocks) != 0 {
		t.Fatalf("full-coverage delta blocks = %d, want 0", len(doc.Blocks))
	}
	if !strings.Contains(rec.Body.String(), `"blocks":[]`) {
		t.Fatalf("empty delta blocks must encode as [], got %s", rec.Body.String())
	}
}

func TestDeltaExportEmptyObject(t *testing.T) {
	h := Handler()
	up := upload(t, h, nil)
	rec := exportDelta(t, h, up.CID, `{"have":[]}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200", rec.Code)
	}
	doc := decodeBundle(t, rec)
	if doc.Root.Size != 0 || len(doc.Root.Chunks) != 0 || len(doc.Blocks) != 0 {
		t.Fatalf("empty object delta = %+v, want no chunks and no blocks", doc)
	}
}

func TestDeltaExportErrors(t *testing.T) {
	h := Handler()
	up := upload(t, h, []byte("delta export errors"))

	req := httptest.NewRequest(http.MethodGet, "/v1/delta-bundles/"+up.CID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	if code := errorCode(t, rec); code != "method_not_allowed" {
		t.Fatalf("GET error = %q, want method_not_allowed", code)
	}

	for _, ct := range []string{"", "text/plain", deltaBundleMediaType, "application/json; charset"} {
		rec = exportDelta(t, h, up.CID, `{"have":[]}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("content type %q: status = %d, want 415", ct, rec.Code)
		}
		if code := errorCode(t, rec); code != "unsupported_media_type" {
			t.Fatalf("content type %q: error = %q, want unsupported_media_type", ct, code)
		}
	}

	rec = exportDelta(t, h, "not-a-cid", `{"have":[]}`, "application/json")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("bad path cid: status = %d code = %q, want 400 invalid_cid", rec.Code, errorCode(t, rec))
	}

	missing := chunkCID([]byte("no such object"))
	rec = exportDelta(t, h, missing, `{"have":[]}`, "application/json")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "object_not_found" {
		t.Fatalf("missing object: status = %d code = %q, want 404 object_not_found", rec.Code, errorCode(t, rec))
	}

	bodies := map[string]string{
		"malformed":        `{"have":`,
		"trailing content": `{"have":[]} extra`,
		"two documents":    `{"have":[]}{"have":[]}`,
		"duplicate key":    `{"have":[],"have":[]}`,
		"unknown field":    `{"have":[],"want":[]}`,
		"missing have":     `{}`,
		"null have":        `{"have":null}`,
		"have not array":   `{"have":"all"}`,
		"entry not string": `{"have":[1]}`,
		"bad cid":          `{"have":["sha256:zz"]}`,
		"duplicate cid":    fmt.Sprintf(`{"have":[%q,%q]}`, up.Chunks[0], up.Chunks[0]),
	}
	for name, body := range bodies {
		rec = exportDelta(t, h, up.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_request", name, rec.Code, errorCode(t, rec))
		}
	}
}

func TestDeltaImportRoundTrip(t *testing.T) {
	hA, hB := Handler(), Handler()
	payload := make([]byte, ChunkSize+777)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := upload(t, hA, payload)

	rec := exportDelta(t, hA, up.CID, `{"have":[]}`, "application/json")
	doc := decodeBundle(t, rec)

	rec = importDelta(t, hB, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", rec.Code)
	}
	resp := decodeObjectResponse(t, rec)
	if !resp.Created || resp.CID != up.CID || resp.Size != len(payload) {
		t.Fatalf("import response = %+v, want created cid=%s size=%d", resp, up.CID, len(payload))
	}

	// The imported object reads back byte-identical with the same manifest.
	rec = httptest.NewRecorder()
	hB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID, nil))
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("imported object body differs from original")
	}
	rec = httptest.NewRecorder()
	hB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID+"/manifest", nil))
	var man manifestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &man); err != nil {
		t.Fatal(err)
	}
	if man.CID != up.CID || man.Size != len(payload) || man.ChunkSize != ChunkSize || len(man.Chunks) != len(up.Chunks) {
		t.Fatalf("manifest = %+v, want cid=%s size=%d chunks=%d", man, up.CID, len(payload), len(up.Chunks))
	}

	// Imported blocks are readable exactly like directly uploaded ones.
	rec = getBlock(t, hB, up.Chunks[0])
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload[:ChunkSize]) {
		t.Fatalf("block read: status = %d, body matches first chunk: %v", rec.Code, bytes.Equal(rec.Body.Bytes(), payload[:ChunkSize]))
	}

	// Reimporting reports the existing object.
	rec = importDelta(t, hB, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusOK {
		t.Fatalf("reimport status = %d, want 200", rec.Code)
	}
	if resp := decodeObjectResponse(t, rec); resp.Created {
		t.Fatal("reimport reported created = true, want false")
	}
}

func TestDeltaImportUsesLocalBlocks(t *testing.T) {
	hA, hB := Handler(), Handler()
	shared := make([]byte, ChunkSize)
	if _, err := rand.Read(shared); err != nil {
		t.Fatal(err)
	}
	// Both instances hold an object containing the shared chunk; A also
	// holds an object that extends it.
	payloadB := append(append([]byte{}, shared...), []byte("only-b")...)
	upB := upload(t, hB, payloadB)
	payloadA := append(append([]byte{}, shared...), []byte("a-tail")...)
	upA := upload(t, hA, payloadA)

	sharedCID := chunkCID(shared)
	rec := exportDelta(t, hA, upA.CID, haveBody(t, []string{sharedCID}), "application/json")
	doc := decodeBundle(t, rec)
	if len(doc.Blocks) != 1 || doc.Blocks[0].CID == sharedCID {
		t.Fatalf("delta blocks = %+v, want only the tail block", doc.Blocks)
	}

	before := getStats(t, hB)
	rec = importDelta(t, hB, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", rec.Code)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.CID != upA.CID {
		t.Fatalf("import cid = %s, want %s", resp.CID, upA.CID)
	}

	rec = httptest.NewRecorder()
	hB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+upA.CID, nil))
	if !bytes.Equal(rec.Body.Bytes(), payloadA) {
		t.Fatal("rebuilt object body differs from original")
	}

	// The shared block is stored once across both objects.
	after := getStats(t, hB)
	if after.Objects != before.Objects+1 {
		t.Fatalf("objects = %d, want %d", after.Objects, before.Objects+1)
	}
	if after.Blocks != before.Blocks+1 {
		t.Fatalf("blocks = %d, want %d (shared block deduplicated)", after.Blocks, before.Blocks+1)
	}
	if after.StoredBytes != before.StoredBytes+len("a-tail") {
		t.Fatalf("storedBytes = %d, want %d", after.StoredBytes, before.StoredBytes+len("a-tail"))
	}
	_ = upB
}

func TestDeltaImportMissingBlock(t *testing.T) {
	hA, hB := Handler(), Handler()
	payload := make([]byte, ChunkSize+10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := upload(t, hA, payload)

	// The delta omits the first chunk; the receiving store does not hold it.
	rec := exportDelta(t, hA, up.CID, haveBody(t, up.Chunks[:1]), "application/json")
	doc := decodeBundle(t, rec)
	if len(doc.Blocks) != 1 {
		t.Fatalf("delta blocks = %d, want 1", len(doc.Blocks))
	}

	before := getStats(t, hB)
	rec = importDelta(t, hB, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "missing_block" {
		t.Fatalf("status = %d code = %q, want 422 missing_block", rec.Code, errorCode(t, rec))
	}
	if after := getStats(t, hB); after != before {
		t.Fatalf("stats changed after failed import: %+v -> %+v", before, after)
	}
}

func TestDeltaImportMediaType(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"", "application/json", bundleMediaType} {
		rec := importDelta(t, h, []byte(`{}`), ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Fatalf("content type %q: status = %d code = %q, want 415 unsupported_media_type",
				ct, rec.Code, errorCode(t, rec))
		}
	}
}

func TestDeltaImportMalformed(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("delta malformed"))
	valid := marshalBundle(t, doc)

	bodies := map[string][]byte{
		"malformed":        []byte(`{"version":`),
		"trailing content": append(valid, []byte(" x")...),
		"two documents":    append(append([]byte{}, valid...), valid...),
		"duplicate key":    []byte(`{"version":1,"version":1,"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]},"blocks":[]}`),
		"unknown field":    []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]},"blocks":[],"extra":1}`),
		"missing version":  []byte(`{"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]},"blocks":[]}`),
		"missing root":     []byte(`{"version":1,"blocks":[]}`),
		"missing blocks":   []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]}}`),
		"version string":   []byte(`{"version":"1","root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]},"blocks":[]}`),
		"size string":      []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `,"size":"0","chunkSize":1,"chunks":[]},"blocks":[]}`),
		"bad root cid":     []byte(`{"version":1,"root":{"cid":"nope","size":0,"chunkSize":1,"chunks":[]},"blocks":[]}`),
		"bad chunk cid":    []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":["nope"]},"blocks":[]}`),
		"bad block cid":    []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `","size":0,"chunkSize":1,"chunks":[]},"blocks":[{"cid":"nope","data":""}]}`),
		"bad base64":       []byte(`{"version":1,"root":{"cid":"` + doc.Root.CID + `","size":1,"chunkSize":1,"chunks":["` + doc.Root.Chunks[0] + `"]},"blocks":[{"cid":"` + doc.Root.Chunks[0] + `","data":"!!!"}]}`),
	}
	for name, body := range bodies {
		rec := importDelta(t, h, body, deltaBundleMediaType)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_delta_bundle" {
			t.Errorf("%s: status = %d code = %q, want 400 invalid_delta_bundle", name, rec.Code, errorCode(t, rec))
		}
	}
}

func TestDeltaImportUnsupported(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("delta unsupported"))

	doc.Version = 2
	rec := importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "unsupported_delta_bundle" {
		t.Fatalf("version 2: status = %d code = %q, want 422 unsupported_delta_bundle", rec.Code, errorCode(t, rec))
	}

	doc.Version = 1
	doc.Root.ChunkSize = ChunkSize * 2
	rec = importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "unsupported_delta_bundle" {
		t.Fatalf("chunkSize: status = %d code = %q, want 422 unsupported_delta_bundle", rec.Code, errorCode(t, rec))
	}
}

func TestDeltaImportTooLarge(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("delta too large"))
	doc.Root.Size = MaxObjectSize + 1
	rec := importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("status = %d code = %q, want 413 payload_too_large", rec.Code, errorCode(t, rec))
	}
}

func TestDeltaImportIntegrityFailures(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize+50)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	base, root := buildBundle(t, payload)

	cases := map[string]func(doc *bundleDocument){
		"digest mismatch": func(doc *bundleDocument) {
			doc.Blocks[0].Data = base64.StdEncoding.EncodeToString([]byte("tampered"))
		},
		"duplicate block": func(doc *bundleDocument) {
			doc.Blocks = append(doc.Blocks, doc.Blocks[0])
		},
		"unreferenced block": func(doc *bundleDocument) {
			extra := []byte("unreferenced")
			doc.Blocks = append(doc.Blocks, bundleBlock{
				CID:  chunkCID(extra),
				Data: base64.StdEncoding.EncodeToString(extra),
			})
		},
		"wrong size": func(doc *bundleDocument) {
			doc.Root.Size = len(payload) - 1
		},
		"wrong root cid": func(doc *bundleDocument) {
			doc.Root.Chunks = append(append([]string{}, doc.Root.Chunks[1:]...), doc.Root.Chunks[0])
		},
		"chunk length": func(doc *bundleDocument) {
			// First chunk must be exactly ChunkSize; shrink it.
			short := payload[:ChunkSize-1]
			doc.Root.Chunks[0] = chunkCID(short)
			doc.Blocks[0] = bundleBlock{CID: chunkCID(short), Data: base64.StdEncoding.EncodeToString(short)}
			doc.Root.Size = len(payload) - 1
			doc.Root.CID = expectedRootCID(doc.Root.Size, doc.Root.Chunks)
		},
	}
	for name, mutate := range cases {
		doc := base
		doc.Root.Chunks = append([]string{}, base.Root.Chunks...)
		doc.Blocks = append([]bundleBlock{}, base.Blocks...)
		mutate(&doc)
		rec := importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
		if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "integrity_check_failed" {
			t.Errorf("%s: status = %d code = %q, want 422 integrity_check_failed", name, rec.Code, errorCode(t, rec))
		}
	}
	_ = root
}

func TestDeltaImportFailureLeavesStoreUntouched(t *testing.T) {
	h := Handler()
	upload(t, h, []byte("still here"))
	before := getStats(t, h)

	doc, _ := buildBundle(t, []byte("never stored"))
	doc.Blocks[0].Data = base64.StdEncoding.EncodeToString([]byte("tampered"))
	importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	importDelta(t, h, []byte(`not json`), deltaBundleMediaType)

	if after := getStats(t, h); after != before {
		t.Fatalf("stats changed after failed imports: %+v -> %+v", before, after)
	}
}

func TestDeltaImportDoesNotPin(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("no pin from delta"))
	rec := importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"pins":[]`) {
		t.Fatalf("pins after delta import = %s, want empty", body)
	}
}

func TestDeltaImportEmptyObject(t *testing.T) {
	h := Handler()
	doc, root := buildBundle(t, nil)
	rec := importDelta(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", rec.Code)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.CID != root || resp.Size != 0 || len(resp.Chunks) != 0 {
		t.Fatalf("empty import response = %+v, want cid=%s size=0 no chunks", resp, root)
	}
}

func TestDeltaImportPreservesManifestOrder(t *testing.T) {
	hA, hB := Handler(), Handler()
	// Repeat one chunk so the manifest carries a duplicate reference.
	chunk := make([]byte, ChunkSize)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	payload := append(append(append([]byte{}, chunk...), chunk...), []byte("tail")...)
	up := upload(t, hA, payload)
	if up.Chunks[0] != up.Chunks[1] {
		t.Fatalf("test payload should repeat its first chunk: %v", up.Chunks)
	}

	rec := exportDelta(t, hA, up.CID, `{"have":[]}`, "application/json")
	doc := decodeBundle(t, rec)
	if len(doc.Blocks) != 2 {
		t.Fatalf("delta blocks = %d, want 2 unique blocks", len(doc.Blocks))
	}
	rec = importDelta(t, hB, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", rec.Code)
	}
	resp := decodeObjectResponse(t, rec)
	if len(resp.Chunks) != 3 || resp.Chunks[0] != up.Chunks[0] || resp.Chunks[1] != up.Chunks[1] || resp.Chunks[2] != up.Chunks[2] {
		t.Fatalf("imported chunks = %v, want manifest order with duplicate preserved", resp.Chunks)
	}
	rec = httptest.NewRecorder()
	hB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID, nil))
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("rebuilt object body differs from original")
	}
}

func TestDeltaImportConcurrent(t *testing.T) {
	hA, hB := Handler(), Handler()
	payload := make([]byte, ChunkSize+1)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := upload(t, hA, payload)
	rec := exportDelta(t, hA, up.CID, `{"have":[]}`, "application/json")
	body := rec.Body.Bytes()

	const n = 8
	var wg sync.WaitGroup
	created := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := importDelta(t, hB, body, deltaBundleMediaType)
			if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Errorf("concurrent import status = %d, want 201 or 200", rec.Code)
			}
			created <- rec.Code == http.StatusCreated
		}()
	}
	wg.Wait()
	close(created)
	first := 0
	for c := range created {
		if c {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("created count = %d, want exactly 1", first)
	}
}

func TestDeltaMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/v1/delta-bundles", "/v1/delta-bundles/" + chunkCID([]byte("x"))} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", method, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
				t.Errorf("%s %s: Allow = %q, want POST", method, path, allow)
			}
			if code := errorCode(t, rec); code != "method_not_allowed" {
				t.Errorf("%s %s: error = %q, want method_not_allowed", method, path, code)
			}
		}
	}
}
