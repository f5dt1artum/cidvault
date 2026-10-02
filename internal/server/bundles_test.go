package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// buildBundle chunks payload under the fixed rules and returns a valid
// bundle document plus the expected root identifier.
func buildBundle(t *testing.T, payload []byte) (bundleDocument, string) {
	t.Helper()
	cids := []string{}
	dataByCID := map[string][]byte{}
	for start := 0; start < len(payload); start += ChunkSize {
		end := start + ChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunk := payload[start:end]
		cid := chunkCID(chunk)
		cids = append(cids, cid)
		dataByCID[cid] = chunk
	}
	root := expectedRootCID(len(payload), cids)
	unique := map[string]bool{}
	doc := bundleDocument{
		Version: bundleVersion,
		Root: bundleRoot{
			CID:       root,
			Size:      len(payload),
			ChunkSize: ChunkSize,
			Chunks:    cids,
		},
		Blocks: []bundleBlock{},
	}
	for _, c := range cids {
		if unique[c] {
			continue
		}
		unique[c] = true
		doc.Blocks = append(doc.Blocks, bundleBlock{
			CID:  c,
			Data: base64.StdEncoding.EncodeToString(dataByCID[c]),
		})
	}
	return doc, root
}

func postBundleBody(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bundles", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getBundleDoc(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundles/"+cid, nil))
	return rec
}

func marshalBundle(t *testing.T, doc bundleDocument) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	return payload.Error.Code
}

func TestBundleExportRoundTrip(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	// Repeat the first chunk at the end so the object has a duplicate block.
	payload = append(payload, payload[:ChunkSize]...)
	up := postBody(t, h, payload, "application/octet-stream")
	if up.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", up.Code)
	}
	obj := decodeObjectResponse(t, up)

	rec := getBundleDoc(t, h, obj.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != bundleMediaType {
		t.Fatalf("content type = %q, want %q", ct, bundleMediaType)
	}
	var doc bundleDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if doc.Root.CID != obj.CID || doc.Root.Size != obj.Size || doc.Root.ChunkSize != ChunkSize {
		t.Fatalf("unexpected root: %+v", doc.Root)
	}
	if len(doc.Root.Chunks) != len(obj.Chunks) {
		t.Fatalf("chunks = %d, want %d", len(doc.Root.Chunks), len(obj.Chunks))
	}
	for i, c := range obj.Chunks {
		if doc.Root.Chunks[i] != c {
			t.Fatalf("chunk %d = %q, want %q", i, doc.Root.Chunks[i], c)
		}
	}
	// Blocks are the unique referenced blocks sorted by cid.
	wantBlocks := map[string]bool{}
	for _, c := range obj.Chunks {
		wantBlocks[c] = true
	}
	if len(doc.Blocks) != len(wantBlocks) {
		t.Fatalf("blocks = %d, want %d unique", len(doc.Blocks), len(wantBlocks))
	}
	for i, b := range doc.Blocks {
		if !wantBlocks[b.CID] {
			t.Fatalf("unexpected block %q", b.CID)
		}
		if i > 0 && doc.Blocks[i-1].CID >= b.CID {
			t.Fatalf("blocks not sorted by cid at %d", i)
		}
		raw, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			t.Fatalf("block %q data is not standard Base64: %v", b.CID, err)
		}
		if chunkCID(raw) != b.CID {
			t.Fatalf("block %q data does not match its digest", b.CID)
		}
	}

	// The exported bundle imports into a fresh instance and reproduces the
	// object byte for byte under the same root identifier.
	h2 := Handler()
	imp := postBundleBody(t, h2, rec.Body.Bytes(), bundleMediaType)
	if imp.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d", imp.Code, http.StatusCreated)
	}
	imported := decodeObjectResponse(t, imp)
	if imported.CID != obj.CID || !imported.Created || imported.Size != obj.Size {
		t.Fatalf("unexpected import response: %+v", imported)
	}
	get := httptest.NewRecorder()
	h2.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/objects/"+imported.CID, nil))
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatalf("reconstructed body differs from original")
	}

	// A second import of the same bundle reports the existing object.
	again := postBundleBody(t, h2, rec.Body.Bytes(), bundleMediaType)
	if again.Code != http.StatusOK {
		t.Fatalf("re-import status = %d, want %d", again.Code, http.StatusOK)
	}
	if resp := decodeObjectResponse(t, again); resp.Created {
		t.Fatalf("re-import reported created = true")
	}
}

func TestBundleExportEmptyObject(t *testing.T) {
	h := Handler()
	up := postBody(t, h, nil, "application/octet-stream")
	obj := decodeObjectResponse(t, up)

	rec := getBundleDoc(t, h, obj.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc bundleDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Root.Size != 0 || len(doc.Root.Chunks) != 0 || len(doc.Blocks) != 0 {
		t.Fatalf("unexpected empty bundle: %+v", doc)
	}
	if !strings.Contains(rec.Body.String(), `"blocks":[]`) {
		t.Fatalf("empty object must export an empty blocks array: %s", rec.Body.String())
	}
}

func TestBundleExportErrors(t *testing.T) {
	h := Handler()
	rec := getBundleDoc(t, h, "not-a-cid")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("invalid cid: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	rec = getBundleDoc(t, h, chunkCID([]byte("absent")))
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "object_not_found" {
		t.Fatalf("missing object: status = %d code = %q", rec.Code, errorCode(t, rec))
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/bundles/"+chunkCID([]byte("x")), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method: status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBundleImportMediaType(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("hello"))
	raw := marshalBundle(t, doc)
	for _, ct := range []string{"application/json", "application/octet-stream", ""} {
		rec := postBundleBody(t, h, raw, ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Fatalf("content type %q: status = %d code = %q", ct, rec.Code, errorCode(t, rec))
		}
	}
	// Parameters such as a charset are acceptable.
	rec := postBundleBody(t, h, raw, bundleMediaType+"; charset=utf-8")
	if rec.Code != http.StatusCreated {
		t.Fatalf("parameterized media type: status = %d", rec.Code)
	}
}

func TestBundleImportMalformed(t *testing.T) {
	valid, _ := buildBundle(t, []byte("hello"))
	validRaw := marshalBundle(t, valid)

	cases := map[string][]byte{
		"not json":         []byte(`{`),
		"trailing content": append(validRaw, []byte(` {}`)...),
		"duplicate key":    []byte(`{"version":1,"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"unknown field":    []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[],"extra":1}`),
		"missing root":     []byte(`{"version":1,"blocks":[]}`),
		"missing blocks":   []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":0,"chunkSize":1048576,"chunks":[]}}`),
		"size not number":  []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":"0","chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"fractional size":  []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1.5,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"bad root cid":     []byte(`{"version":1,"root":{"cid":"sha256:xyz","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"bad chunk cid":    []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1,"chunkSize":1048576,"chunks":["nope"]},"blocks":[]}`),
		"bad base64":       []byte(`{"version":1,"root":{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1,"chunkSize":1048576,"chunks":["sha256:0000000000000000000000000000000000000000000000000000000000000000"]},"blocks":[{"cid":"sha256:0000000000000000000000000000000000000000000000000000000000000000","data":"!!!"}]}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postBundleBody(t, h, body, bundleMediaType)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_bundle" {
				t.Fatalf("status = %d code = %q, want 400 invalid_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

// mutateBundle rewrites a valid bundle's JSON with the given patch applied
// to the decoded map, returning the re-encoded body.
func mutateBundle(t *testing.T, doc bundleDocument, patch func(map[string]any)) []byte {
	t.Helper()
	raw := marshalBundle(t, doc)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	patch(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBundleImportUnsupported(t *testing.T) {
	payload := []byte("hello")
	doc, _ := buildBundle(t, payload)

	for name, patch := range map[string]func(map[string]any){
		"version 2": func(m map[string]any) { m["version"] = 2 },
		"chunk size": func(m map[string]any) {
			m["root"].(map[string]any)["chunkSize"] = ChunkSize * 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postBundleBody(t, h, mutateBundle(t, doc, patch), bundleMediaType)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "unsupported_bundle" {
				t.Fatalf("status = %d code = %q, want 422 unsupported_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestBundleImportTooLarge(t *testing.T) {
	doc, _ := buildBundle(t, []byte("hello"))
	h := Handler()
	rec := postBundleBody(t, h, mutateBundle(t, doc, func(m map[string]any) {
		m["root"].(map[string]any)["size"] = MaxObjectSize + 1
	}), bundleMediaType)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("status = %d code = %q, want 413 payload_too_large", rec.Code, errorCode(t, rec))
	}
}

func TestBundleImportIntegrityFailures(t *testing.T) {
	payload := make([]byte, ChunkSize+10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	doc, _ := buildBundle(t, payload)
	other := chunkCID([]byte("unreferenced"))

	cases := map[string]func(map[string]any){
		"root cid mismatch": func(m map[string]any) {
			m["root"].(map[string]any)["cid"] = chunkCID([]byte("wrong"))
		},
		"size mismatch": func(m map[string]any) {
			m["root"].(map[string]any)["size"] = len(payload) - 1
		},
		"missing block": func(m map[string]any) {
			m["blocks"] = m["blocks"].([]any)[:1]
		},
		"duplicate block": func(m map[string]any) {
			blocks := m["blocks"].([]any)
			m["blocks"] = append(blocks, blocks[0])
		},
		"unreferenced block": func(m map[string]any) {
			m["blocks"] = append(m["blocks"].([]any), map[string]any{
				"cid":  other,
				"data": base64.StdEncoding.EncodeToString([]byte("unreferenced")),
			})
		},
		"digest mismatch": func(m map[string]any) {
			block := m["blocks"].([]any)[0].(map[string]any)
			block["data"] = base64.StdEncoding.EncodeToString([]byte("tampered"))
		},
		"short middle chunk": func(m map[string]any) {
			// Reference the 10-byte tail block as the first chunk.
			chunks := m["root"].(map[string]any)["chunks"].([]any)
			chunks[0], chunks[1] = chunks[1], chunks[0]
		},
		"empty object with chunks": func(m map[string]any) {
			m["root"].(map[string]any)["size"] = 0
		},
		"empty object without chunks": func(m map[string]any) {
			m["root"].(map[string]any)["size"] = 0
			m["root"].(map[string]any)["chunks"] = []any{}
			m["blocks"] = []any{}
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postBundleBody(t, h, mutateBundle(t, doc, patch), bundleMediaType)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "integrity_check_failed" {
				t.Fatalf("status = %d code = %q, want 422 integrity_check_failed", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestBundleImportFailureLeavesStoreUntouched(t *testing.T) {
	h := Handler()
	up := postBody(t, h, []byte("keep me"), "application/octet-stream")
	if up.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", up.Code)
	}
	before := httptest.NewRecorder()
	h.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))

	doc, _ := buildBundle(t, []byte("bad import"))
	bad := mutateBundle(t, doc, func(m map[string]any) {
		m["root"].(map[string]any)["cid"] = chunkCID([]byte("wrong"))
	})
	rec := postBundleBody(t, h, bad, bundleMediaType)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}

	after := httptest.NewRecorder()
	h.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	if before.Body.String() != after.Body.String() {
		t.Fatalf("stats changed after failed import: %s -> %s", before.Body, after.Body)
	}
}

func TestBundleImportDoesNotPin(t *testing.T) {
	h := Handler()
	doc, root := buildBundle(t, []byte("collect me"))
	rec := postBundleBody(t, h, marshalBundle(t, doc), bundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import status = %d", rec.Code)
	}
	gc := httptest.NewRecorder()
	h.ServeHTTP(gc, httptest.NewRequest(http.MethodPost, "/v1/gc", nil))
	var gcResp gcResponse
	if err := json.Unmarshal(gc.Body.Bytes(), &gcResp); err != nil {
		t.Fatal(err)
	}
	if gcResp.Objects != 1 || gcResp.CIDs[0] != root {
		t.Fatalf("imported object was not collected: %+v", gcResp)
	}
}

func TestBundleImportSharesBlocks(t *testing.T) {
	h := Handler()
	payload := []byte("shared block body")
	up := postBody(t, h, payload, "application/octet-stream")
	obj := decodeObjectResponse(t, up)

	// Import the same object as a bundle: no new blocks are stored.
	doc, _ := buildBundle(t, payload)
	rec := postBundleBody(t, h, marshalBundle(t, doc), bundleMediaType)
	if rec.Code != http.StatusOK || decodeObjectResponse(t, rec).Created {
		t.Fatalf("status = %d, want 200 with created = false", rec.Code)
	}
	stats := httptest.NewRecorder()
	h.ServeHTTP(stats, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var st statsResponse
	if err := json.Unmarshal(stats.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Objects != 1 || st.Blocks != 1 || st.StoredBytes != len(payload) {
		t.Fatalf("unexpected stats after shared import: %+v (cid %s)", st, obj.CID)
	}
}

func TestBundleImportEmptyObject(t *testing.T) {
	h := Handler()
	doc, root := buildBundle(t, nil)
	rec := postBundleBody(t, h, marshalBundle(t, doc), bundleMediaType)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.CID != root || resp.Size != 0 || len(resp.Chunks) != 0 || !resp.Created {
		t.Fatalf("unexpected response: %+v", resp)
	}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/objects/"+root, nil))
	if get.Code != http.StatusOK || get.Body.Len() != 0 {
		t.Fatalf("empty object read: status = %d len = %d", get.Code, get.Body.Len())
	}
}

func TestBundleImportEdgeNumbers(t *testing.T) {
	emptyRoot := expectedRootCID(0, []string{})
	body := func(version, size string) []byte {
		return []byte(fmt.Sprintf(
			`{"version":%s,"root":{"cid":%q,"size":%s,"chunkSize":%d,"chunks":[]},"blocks":[]}`,
			version, emptyRoot, size, ChunkSize))
	}
	cases := []struct {
		name       string
		body       []byte
		wantStatus int
		wantCode   string
	}{
		{"huge size", body("1", "99999999999999999999"), http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"huge version", body("99999999999999999999", "0"), http.StatusUnprocessableEntity, "unsupported_bundle"},
		{"negative size", body("1", "-1"), http.StatusUnprocessableEntity, "integrity_check_failed"},
		{"negative version", body("-1", "0"), http.StatusUnprocessableEntity, "unsupported_bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler()
			rec := postBundleBody(t, h, tc.body, bundleMediaType)
			if rec.Code != tc.wantStatus || errorCode(t, rec) != tc.wantCode {
				t.Fatalf("status = %d code = %q, want %d %s",
					rec.Code, errorCode(t, rec), tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestBundleMethodNotAllowed(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/bundles", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBundleImportConcurrent(t *testing.T) {
	h := Handler()
	doc, _ := buildBundle(t, []byte("race me"))
	raw := marshalBundle(t, doc)

	const n = 8
	codes := make(chan int, n)
	created := make(chan bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postBundleBody(t, h, raw, bundleMediaType)
			codes <- rec.Code
			created <- decodeObjectResponse(t, rec).Created
		}()
	}
	wg.Wait()
	close(codes)
	close(created)
	createdCount := 0
	for c := range created {
		if c {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want exactly 1", createdCount)
	}
	status201 := 0
	for code := range codes {
		if code == http.StatusCreated {
			status201++
		} else if code != http.StatusOK {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if status201 != 1 {
		t.Fatalf("201 count = %d, want exactly 1", status201)
	}
}
