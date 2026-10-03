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

// buildDeltaDoc chunks payload under the fixed rules and returns a delta
// document carrying every unique block except those listed in have, along
// with the expected root identifier.
func buildDeltaDoc(t *testing.T, payload []byte, have ...string) (bundleDocument, string) {
	t.Helper()
	doc, root := buildBundle(t, payload)
	held := map[string]bool{}
	for _, c := range have {
		held[c] = true
	}
	blocks := doc.Blocks[:0]
	for _, b := range doc.Blocks {
		if !held[b.CID] {
			blocks = append(blocks, b)
		}
	}
	doc.Blocks = blocks
	return doc, root
}

func postDeltaRequest(t *testing.T, h http.Handler, cid string, have any, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	switch v := have.(type) {
	case nil:
		body = []byte(`{"have":[]}`)
	case []byte:
		body = v
	default:
		raw, err := json.Marshal(map[string]any{"have": have})
		if err != nil {
			t.Fatal(err)
		}
		body = raw
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/delta-bundles/"+cid, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postDeltaBody(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/delta-bundles", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// chunkOf returns the chunk bytes at index i of payload split by ChunkSize.
func chunkOf(payload []byte, i int) []byte {
	start := i * ChunkSize
	end := start + ChunkSize
	if end > len(payload) {
		end = len(payload)
	}
	return payload[start:end]
}

func TestDeltaExportFiltersHave(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
	c1, c2, c3 := obj.Chunks[0], obj.Chunks[1], obj.Chunks[2]

	// Listing the first block leaves the other two unique blocks, sorted.
	rec := postDeltaRequest(t, h, obj.CID, []string{c1}, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != deltaBundleMediaType {
		t.Fatalf("content type = %q, want %q", ct, deltaBundleMediaType)
	}
	var doc bundleDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.Root.CID != obj.CID || doc.Root.Size != obj.Size ||
		doc.Root.ChunkSize != ChunkSize {
		t.Fatalf("unexpected root: %+v", doc.Root)
	}
	if strings.Join(doc.Root.Chunks, ",") != strings.Join(obj.Chunks, ",") {
		t.Fatalf("manifest chunks differ: %+v", doc.Root.Chunks)
	}
	want := []string{c2, c3}
	sort.Strings(want)
	if len(doc.Blocks) != len(want) {
		t.Fatalf("blocks = %d, want %d", len(doc.Blocks), len(want))
	}
	for i, c := range want {
		if doc.Blocks[i].CID != c {
			t.Fatalf("block %d = %q, want %q", i, doc.Blocks[i].CID, c)
		}
		if i > 0 && doc.Blocks[i-1].CID >= doc.Blocks[i].CID {
			t.Fatalf("blocks not sorted at %d", i)
		}
	}

	// Irrelevant identifiers are ignored; an all-hit request ships nothing.
	irrelevant := chunkCID([]byte("something else entirely"))
	rec = postDeltaRequest(t, h, obj.CID, []string{irrelevant}, "application/json")
	if rec.Code != http.StatusOK || len(decodeBundle(t, rec).Blocks) != 3 {
		t.Fatalf("irrelevant have must be ignored: status = %d", rec.Code)
	}
	rec = postDeltaRequest(t, h, obj.CID, []string{c1, c2, c3}, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if all := decodeBundle(t, rec); len(all.Blocks) != 0 ||
		!strings.Contains(rec.Body.String(), `"blocks":[]`) {
		t.Fatalf("all-hit export must have empty blocks: %s", rec.Body.String())
	}
}

func decodeBundle(t *testing.T, rec *httptest.ResponseRecorder) bundleDocument {
	t.Helper()
	var doc bundleDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body is not a bundle: %v", err)
	}
	return doc
}

func TestDeltaExportEmptyObject(t *testing.T) {
	h := Handler()
	obj := decodeObjectResponse(t, postBody(t, h, nil, "application/octet-stream"))
	rec := postDeltaRequest(t, h, obj.CID, []string{}, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	doc := decodeBundle(t, rec)
	if doc.Root.Size != 0 || len(doc.Root.Chunks) != 0 || len(doc.Blocks) != 0 {
		t.Fatalf("unexpected empty delta: %+v", doc)
	}
}

func TestDeltaExportErrors(t *testing.T) {
	h := Handler()
	obj := decodeObjectResponse(t, postBody(t, h, []byte("hello world"), "application/octet-stream"))

	// Path identifier and existence.
	rec := postDeltaRequest(t, h, "not-a-cid", []string{}, "application/json")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("bad cid: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	rec = postDeltaRequest(t, h, chunkCID([]byte("absent object")), []string{}, "application/json")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "object_not_found" {
		t.Fatalf("missing object: status = %d code = %q", rec.Code, errorCode(t, rec))
	}

	// Media type.
	for _, ct := range []string{bundleMediaType, "application/octet-stream", ""} {
		rec = postDeltaRequest(t, h, obj.CID, []string{}, ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Fatalf("content type %q: status = %d code = %q", ct, rec.Code, errorCode(t, rec))
		}
	}

	// Malformed request bodies.
	bad := map[string][]byte{
		"not json":         []byte(`{`),
		"trailing":         []byte(`{"have":[]} {}`),
		"duplicate key":    []byte(`{"have":[],"have":[]}`),
		"unknown field":    []byte(`{"have":[],"extra":1}`),
		"missing have":     []byte(`{}`),
		"have not array":   []byte(`{"have":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`),
		"entry not string": []byte(`{"have":[1]}`),
		"bad cid":          []byte(`{"have":["nope"]}`),
		"duplicate cid": []byte(fmt.Sprintf(`{"have":["sha256:%[1]s","sha256:%[1]s"]}`,
			strings.Repeat("a", 64))),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			rec := postDeltaRequest(t, h, obj.CID, body, "application/json")
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
				t.Fatalf("status = %d code = %q, want 400 invalid_request", rec.Code, errorCode(t, rec))
			}
		})
	}

	// Method not allowed.
	req := httptest.NewRequest(http.MethodGet, "/v1/delta-bundles/"+obj.CID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET item: status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestDeltaImportReconstructsWithLocalBlocks(t *testing.T) {
	sender := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := decodeObjectResponse(t, postBody(t, sender, payload, "application/octet-stream"))
	c1 := obj.Chunks[0]

	// Receiver only holds the first block, via a different object that shares it.
	receiver := Handler()
	carrier := decodeObjectResponse(t, postBody(t, receiver, chunkOf(payload, 0), "application/octet-stream"))
	if carrier.Chunks[0] != c1 {
		t.Fatal("carrier does not share the first block")
	}

	// Sender ships the blocks the receiver lacks.
	delta := postDeltaRequest(t, sender, obj.CID, []string{c1}, "application/json")
	imp := postDeltaBody(t, receiver, delta.Body.Bytes(), deltaBundleMediaType)
	if imp.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want 201", imp.Code)
	}
	resp := decodeObjectResponse(t, imp)
	if resp.CID != obj.CID || !resp.Created || resp.Size != obj.Size {
		t.Fatalf("unexpected import response: %+v", resp)
	}
	if strings.Join(resp.Chunks, ",") != strings.Join(obj.Chunks, ",") {
		t.Fatalf("chunks = %+v, want %+v", resp.Chunks, obj.Chunks)
	}

	// The reconstructed object reads byte for byte and the manifest matches.
	get := httptest.NewRecorder()
	receiver.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatal("reconstructed body differs from the original")
	}
	man := httptest.NewRecorder()
	receiver.ServeHTTP(man, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID+"/manifest", nil))
	var gotManifest manifestResponse
	if err := json.Unmarshal(man.Body.Bytes(), &gotManifest); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotManifest.Chunks, ",") != strings.Join(obj.Chunks, ",") {
		t.Fatalf("manifest chunks differ: %+v", gotManifest.Chunks)
	}

	// Blocks stay deduplicated: the shared first block is stored once and
	// every block remains readable.
	stats := getStats(t, receiver)
	if stats.Objects != 2 || stats.Blocks != 3 ||
		stats.LogicalBytes != len(payload)+ChunkSize ||
		stats.StoredBytes != len(payload) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	for _, c := range obj.Chunks {
		blk := httptest.NewRecorder()
		receiver.ServeHTTP(blk, httptest.NewRequest(http.MethodGet, "/v1/blocks/"+c, nil))
		if blk.Code != http.StatusOK {
			t.Fatalf("block %s not readable: %d", c, blk.Code)
		}
	}

	// Re-importing reports the existing object.
	again := postDeltaBody(t, receiver, delta.Body.Bytes(), deltaBundleMediaType)
	if again.Code != http.StatusOK || decodeObjectResponse(t, again).Created {
		t.Fatalf("re-import: status = %d created = true", again.Code)
	}
}

func TestDeltaImportEmptyBlocksWhenAllLocal(t *testing.T) {
	sender := Handler()
	payload := make([]byte, ChunkSize*2+7)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := decodeObjectResponse(t, postBody(t, sender, payload, "application/octet-stream"))

	// Receiver holds every referenced block through two other objects, but
	// not the object itself: C1 alone and C2||C3tail together.
	receiver := Handler()
	postBody(t, receiver, chunkOf(payload, 0), "application/octet-stream")
	postBody(t, receiver, append(append([]byte{}, chunkOf(payload, 1)...), chunkOf(payload, 2)...), "application/octet-stream")

	delta := postDeltaRequest(t, sender, obj.CID, obj.Chunks, "application/json")
	if len(decodeBundle(t, delta).Blocks) != 0 {
		t.Fatal("expected an empty-blocks delta")
	}
	imp := postDeltaBody(t, receiver, delta.Body.Bytes(), deltaBundleMediaType)
	if imp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", imp.Code)
	}
	get := httptest.NewRecorder()
	receiver.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatal("reconstructed body differs")
	}
}

func TestDeltaImportPreservesDuplicateReferences(t *testing.T) {
	sender := Handler()
	head := make([]byte, ChunkSize)
	if _, err := rand.Read(head); err != nil {
		t.Fatal(err)
	}
	mid := make([]byte, ChunkSize)
	if _, err := rand.Read(mid); err != nil {
		t.Fatal(err)
	}
	// C1 || C2 || C1: the first block is referenced twice, both full chunks.
	payload := append(append(append([]byte{}, head...), mid...), head...)
	obj := decodeObjectResponse(t, postBody(t, sender, payload, "application/octet-stream"))
	if len(obj.Chunks) != 3 || obj.Chunks[0] != obj.Chunks[2] {
		t.Fatalf("expected a repeated first chunk, got %+v", obj.Chunks)
	}

	receiver := Handler()
	postBody(t, receiver, head, "application/octet-stream") // holds the repeated block
	delta := postDeltaRequest(t, sender, obj.CID, []string{obj.Chunks[0]}, "application/json")
	if shipped := decodeBundle(t, delta).Blocks; len(shipped) != 1 || shipped[0].CID != obj.Chunks[1] {
		t.Fatalf("expected only the middle block shipped, got %+v", shipped)
	}
	imp := postDeltaBody(t, receiver, delta.Body.Bytes(), deltaBundleMediaType)
	if imp.Code != http.StatusCreated {
		t.Fatalf("status = %d", imp.Code)
	}
	get := httptest.NewRecorder()
	receiver.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatal("reconstructed body differs")
	}
	st := getStats(t, receiver)
	if st.Objects != 2 || st.Blocks != 2 { // two unique blocks despite three references
		t.Fatalf("objects = %d blocks = %d, want 2 and 2", st.Objects, st.Blocks)
	}
}

func TestDeltaImportEmptyObject(t *testing.T) {
	doc, root := buildDeltaDoc(t, nil)
	h := Handler()
	rec := postDeltaBody(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.CID != root || resp.Size != 0 || len(resp.Chunks) != 0 || !resp.Created {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestDeltaImportMissingBlock(t *testing.T) {
	payload := make([]byte, ChunkSize+9)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	// Carry only the tail block; the first block is neither bundled nor local.
	doc, _ := buildDeltaDoc(t, payload)
	doc.Blocks = doc.Blocks[1:]

	h := Handler()
	rec := postDeltaBody(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "missing_block" {
		t.Fatalf("status = %d code = %q, want 422 missing_block", rec.Code, errorCode(t, rec))
	}
	if st := getStats(t, h); st != (statsResponse{}) {
		t.Fatalf("failed import changed stats: %+v", st)
	}
}

func TestDeltaImportMediaType(t *testing.T) {
	doc, _ := buildDeltaDoc(t, nil)
	raw := marshalBundle(t, doc)
	h := Handler()
	for _, ct := range []string{"application/json", bundleMediaType, "application/octet-stream", ""} {
		rec := postDeltaBody(t, h, raw, ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Fatalf("content type %q: status = %d code = %q", ct, rec.Code, errorCode(t, rec))
		}
	}
	rec := postDeltaBody(t, h, raw, deltaBundleMediaType+"; charset=utf-8")
	if rec.Code != http.StatusCreated {
		t.Fatalf("parameterized media type: status = %d", rec.Code)
	}
}

func TestDeltaImportMalformed(t *testing.T) {
	zero := strings.Repeat("0", 64)
	cases := map[string][]byte{
		"not json":         []byte(`{`),
		"trailing content": []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]} {}`),
		"duplicate key":    []byte(`{"version":1,"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"unknown field":    []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[],"x":1}`),
		"missing root":     []byte(`{"version":1,"blocks":[]}`),
		"missing blocks":   []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":[]}}`),
		"size string":      []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":"0","chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"fractional size":  []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":1.2,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"bad root cid":     []byte(`{"version":1,"root":{"cid":"sha256:xyz","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"bad chunk cid":    []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":["nope"]},"blocks":[]}`),
		"bad block cid":    []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[{"cid":"bad","data":""}]}`),
		"bad base64":       []byte(`{"version":1,"root":{"cid":"sha256:` + zero + `","size":1,"chunkSize":1048576,"chunks":["sha256:` + zero + `"]},"blocks":[{"cid":"sha256:` + zero + `","data":"%%%"}]}`),
		"null body":        []byte(`null`),
		"array body":       []byte(`[]`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postDeltaBody(t, h, body, deltaBundleMediaType)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_delta_bundle" {
				t.Fatalf("status = %d code = %q, want 400 invalid_delta_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestDeltaImportUnsupported(t *testing.T) {
	doc, _ := buildDeltaDoc(t, []byte("hello"))
	for name, patch := range map[string]func(map[string]any){
		"version 2": func(m map[string]any) { m["version"] = 2 },
		"chunk size": func(m map[string]any) {
			m["root"].(map[string]any)["chunkSize"] = ChunkSize * 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postDeltaBody(t, h, mutateBundle(t, doc, patch), deltaBundleMediaType)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "unsupported_delta_bundle" {
				t.Fatalf("status = %d code = %q, want 422 unsupported_delta_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestDeltaImportTooLarge(t *testing.T) {
	doc, _ := buildDeltaDoc(t, nil)
	h := Handler()
	rec := postDeltaBody(t, h, mutateBundle(t, doc, func(m map[string]any) {
		m["root"].(map[string]any)["size"] = MaxObjectSize + 1
	}), deltaBundleMediaType)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("status = %d code = %q, want 413 payload_too_large", rec.Code, errorCode(t, rec))
	}
}

func TestDeltaImportIntegrityFailures(t *testing.T) {
	payload := make([]byte, ChunkSize+10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(map[string]any){
		"root cid mismatch": func(m map[string]any) {
			m["root"].(map[string]any)["cid"] = chunkCID([]byte("wrong"))
		},
		"size mismatch": func(m map[string]any) {
			m["root"].(map[string]any)["size"] = len(payload) - 1
		},
		"duplicate block": func(m map[string]any) {
			blocks := m["blocks"].([]any)
			m["blocks"] = append(blocks, blocks[0])
		},
		"unreferenced block": func(m map[string]any) {
			m["blocks"] = append(m["blocks"].([]any), map[string]any{
				"cid":  chunkCID([]byte("unreferenced")),
				"data": base64.StdEncoding.EncodeToString([]byte("unreferenced")),
			})
		},
		"digest mismatch": func(m map[string]any) {
			m["blocks"].([]any)[0].(map[string]any)["data"] =
				base64.StdEncoding.EncodeToString([]byte("tampered"))
		},
		"short middle chunk": func(m map[string]any) {
			chunks := m["root"].(map[string]any)["chunks"].([]any)
			chunks[0], chunks[1] = chunks[1], chunks[0]
		},
		"negative size": func(m map[string]any) {
			m["root"].(map[string]any)["size"] = -1
		},
		"empty object with blocks": func(m map[string]any) {
			root := m["root"].(map[string]any)
			root["size"] = 0
			root["chunks"] = []any{}
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			doc, _ := buildDeltaDoc(t, payload)
			h := Handler()
			rec := postDeltaBody(t, h, mutateBundle(t, doc, patch), deltaBundleMediaType)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "integrity_check_failed" {
				t.Fatalf("status = %d code = %q, want 422 integrity_check_failed", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestDeltaImportFailureLeavesStoreUntouched(t *testing.T) {
	h := Handler()
	postBody(t, h, []byte("keep me"), "application/octet-stream")
	before := httptest.NewRecorder()
	h.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))

	doc, _ := buildDeltaDoc(t, []byte("bad import"))
	bad := mutateBundle(t, doc, func(m map[string]any) {
		m["root"].(map[string]any)["cid"] = chunkCID([]byte("wrong"))
	})
	if rec := postDeltaBody(t, h, bad, deltaBundleMediaType); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}

	after := httptest.NewRecorder()
	h.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	if before.Body.String() != after.Body.String() {
		t.Fatalf("stats changed: %s -> %s", before.Body, after.Body)
	}
}

func TestDeltaImportDoesNotPin(t *testing.T) {
	doc, root := buildDeltaDoc(t, []byte("collect me"))
	h := Handler()
	rec := postDeltaBody(t, h, marshalBundle(t, doc), deltaBundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d", rec.Code)
	}
	gc := httptest.NewRecorder()
	h.ServeHTTP(gc, httptest.NewRequest(http.MethodPost, "/v1/gc", nil))
	var resp gcResponse
	if err := json.Unmarshal(gc.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Objects != 1 || resp.CIDs[0] != root {
		t.Fatalf("imported object was not collected: %+v", resp)
	}
}

func TestDeltaImportMethodNotAllowed(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/delta-bundles", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestDeltaImportConcurrent(t *testing.T) {
	sender := Handler()
	payload := make([]byte, ChunkSize+33)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := decodeObjectResponse(t, postBody(t, sender, payload, "application/octet-stream"))

	receiver := Handler()
	postBody(t, receiver, chunkOf(payload, 0), "application/octet-stream") // local first block
	delta := postDeltaRequest(t, sender, obj.CID, []string{obj.Chunks[0]}, "application/json")
	raw := delta.Body.Bytes()

	const n = 8
	var wg sync.WaitGroup
	created := make(chan bool, n)
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postDeltaBody(t, receiver, raw, deltaBundleMediaType)
			codes <- rec.Code
			created <- decodeObjectResponse(t, rec).Created
		}()
	}
	wg.Wait()
	close(created)
	close(codes)
	createdCount, status201 := 0, 0
	for c := range created {
		if c {
			createdCount++
		}
	}
	for code := range codes {
		if code == http.StatusCreated {
			status201++
		} else if code != http.StatusOK {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if createdCount != 1 || status201 != 1 {
		t.Fatalf("created = %d, 201 = %d, want exactly 1 each", createdCount, status201)
	}
}
