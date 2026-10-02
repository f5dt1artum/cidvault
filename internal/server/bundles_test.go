package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bundleJSON is a test-side view of the bundle document.
type bundleJSON struct {
	Version int `json:"version"`
	Root    struct {
		CID       string   `json:"cid"`
		Size      int      `json:"size"`
		ChunkSize int      `json:"chunkSize"`
		Chunks    []string `json:"chunks"`
	} `json:"root"`
	Blocks []struct {
		CID  string `json:"cid"`
		Data string `json:"data"`
	} `json:"blocks"`
}

func uploadObject(t *testing.T, h http.Handler, body []byte) objectResponse {
	t.Helper()
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusCreated)
	}
	return decodeObjectResponse(t, rec)
}

func getBundle(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundles/"+cid, nil))
	return rec
}

func postBundle(t *testing.T, h http.Handler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/bundles", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBundle(t *testing.T, rec *httptest.ResponseRecorder) bundleJSON {
	t.Helper()
	var doc bundleJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	return doc
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

// buildBundle re-encodes a bundle document from parts, keeping chunk order
// and duplicates and sorting unique blocks by cid.
func buildBundle(t *testing.T, size int, chunks []string, blockData map[string][]byte) []byte {
	t.Helper()
	cids := make([]string, 0, len(blockData))
	for c := range blockData {
		cids = append(cids, c)
	}
	for i := 0; i+1 < len(cids); i++ {
		for j := i + 1; j < len(cids); j++ {
			if cids[j] < cids[i] {
				cids[i], cids[j] = cids[j], cids[i]
			}
		}
	}
	var b strings.Builder
	b.WriteString(`{"version":1,"root":{"cid":`)
	root := expectedRootCID(size, chunks)
	fmt.Fprintf(&b, "%q,", root)
	fmt.Fprintf(&b, `"size":%d,"chunkSize":%d,"chunks":[`, size, ChunkSize)
	for i, c := range chunks {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", c)
	}
	b.WriteString(`]},"blocks":[`)
	for i, c := range cids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"cid":%q,"data":%q}`, c, base64.StdEncoding.EncodeToString(blockData[c]))
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func chunkData(body []byte) ([]string, map[string][]byte) {
	chunks := []string{}
	data := map[string][]byte{}
	for start := 0; start < len(body); start += ChunkSize {
		end := start + ChunkSize
		if end > len(body) {
			end = len(body)
		}
		chunk := body[start:end]
		sum := sha256.Sum256(chunk)
		cid := "sha256:" + hex.EncodeToString(sum[:])
		chunks = append(chunks, cid)
		data[cid] = chunk
	}
	return chunks, data
}

func TestBundleExportRoundTrip(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*2+123)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := uploadObject(t, h, payload)

	rec := getBundle(t, h, up.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.cidvault.bundle+json" {
		t.Fatalf("Content-Type = %q, want bundle media type", ct)
	}
	doc := decodeBundle(t, rec)
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if doc.Root.CID != up.CID || doc.Root.Size != len(payload) || doc.Root.ChunkSize != ChunkSize {
		t.Fatalf("unexpected root: %+v", doc.Root)
	}
	if len(doc.Root.Chunks) != len(up.Chunks) {
		t.Fatalf("chunks = %d, want %d", len(doc.Root.Chunks), len(up.Chunks))
	}
	for i, c := range up.Chunks {
		if doc.Root.Chunks[i] != c {
			t.Fatalf("chunk %d = %q, want %q", i, doc.Root.Chunks[i], c)
		}
	}
	if len(doc.Blocks) != len(up.Chunks) {
		t.Fatalf("blocks = %d, want %d", len(doc.Blocks), len(up.Chunks))
	}
	for i := 1; i < len(doc.Blocks); i++ {
		if doc.Blocks[i-1].CID >= doc.Blocks[i].CID {
			t.Fatalf("blocks not sorted by cid: %q then %q", doc.Blocks[i-1].CID, doc.Blocks[i].CID)
		}
	}
	var rebuilt []byte
	byCID := map[string][]byte{}
	for _, b := range doc.Blocks {
		raw, err := base64.StdEncoding.DecodeString(b.Data)
		if err != nil {
			t.Fatalf("block %s is not standard Base64: %v", b.CID, err)
		}
		sum := sha256.Sum256(raw)
		if want := "sha256:" + hex.EncodeToString(sum[:]); b.CID != want {
			t.Fatalf("block cid = %q, want %q", b.CID, want)
		}
		byCID[b.CID] = raw
	}
	for _, c := range up.Chunks {
		rebuilt = append(rebuilt, byCID[c]...)
	}
	if !bytes.Equal(rebuilt, payload) {
		t.Fatal("rebuilt payload differs from upload")
	}
}

func TestBundleExportEmptyObject(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, nil)
	rec := getBundle(t, h, up.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	doc := decodeBundle(t, rec)
	if doc.Root.Size != 0 || len(doc.Root.Chunks) != 0 {
		t.Fatalf("unexpected empty root: %+v", doc.Root)
	}
	if doc.Blocks == nil || len(doc.Blocks) != 0 {
		t.Fatalf("blocks = %v, want empty array", doc.Blocks)
	}
	if !strings.Contains(rec.Body.String(), `"blocks":[]`) {
		t.Fatalf("empty object must export an empty blocks array: %s", rec.Body.String())
	}
}

func TestBundleExportDeduplicatesBlocks(t *testing.T) {
	h := Handler()
	chunk := bytes.Repeat([]byte("a"), ChunkSize)
	payload := append(append([]byte{}, chunk...), chunk...)
	up := uploadObject(t, h, payload)
	if len(up.Chunks) != 2 || up.Chunks[0] != up.Chunks[1] {
		t.Fatalf("expected two identical chunks, got %v", up.Chunks)
	}
	doc := decodeBundle(t, getBundle(t, h, up.CID))
	if len(doc.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 unique block", len(doc.Blocks))
	}
	if doc.Blocks[0].CID != up.Chunks[0] {
		t.Fatalf("block cid = %q, want %q", doc.Blocks[0].CID, up.Chunks[0])
	}
}

func TestBundleExportErrors(t *testing.T) {
	h := Handler()
	rec := getBundle(t, h, "not-a-cid")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("invalid cid: status = %d, code = %q", rec.Code, errorCode(t, rec))
	}
	missing := expectedRootCID(0, nil)
	rec = getBundle(t, h, missing)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "object_not_found" {
		t.Fatalf("missing object: status = %d, code = %q", rec.Code, errorCode(t, rec))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/bundles/"+missing, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("method not allowed: status = %d, Allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBundleImportExportRoundTrip(t *testing.T) {
	src := Handler()
	payload := make([]byte, ChunkSize+777)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := uploadObject(t, src, payload)
	bundle := getBundle(t, src, up.CID).Body.Bytes()

	dst := Handler()
	rec := postBundle(t, dst, bundle, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeObjectResponse(t, rec)
	if resp.CID != up.CID || resp.Size != up.Size || resp.ChunkSize != ChunkSize || !resp.Created {
		t.Fatalf("unexpected import response: %+v", resp)
	}
	if len(resp.Chunks) != len(up.Chunks) {
		t.Fatalf("chunks = %d, want %d", len(resp.Chunks), len(up.Chunks))
	}
	for i, c := range up.Chunks {
		if resp.Chunks[i] != c {
			t.Fatalf("chunk %d = %q, want %q", i, resp.Chunks[i], c)
		}
	}

	// The imported object reads back byte-identical.
	rec = httptest.NewRecorder()
	dst.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID, nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("imported object mismatch: status = %d", rec.Code)
	}

	// Re-importing reports the existing object.
	rec = postBundle(t, dst, bundle, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-import status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp := decodeObjectResponse(t, rec); resp.Created {
		t.Fatalf("re-import created = true, want false")
	}
}

func TestBundleImportEmptyObject(t *testing.T) {
	h := Handler()
	bundle := []byte(`{"version":1,"root":{"cid":` +
		fmt.Sprintf("%q", expectedRootCID(0, nil)) +
		`,"size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`)
	rec := postBundle(t, h, bundle, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeObjectResponse(t, rec)
	if resp.Size != 0 || len(resp.Chunks) != 0 || !resp.Created {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestBundleImportDoesNotPin(t *testing.T) {
	h := Handler()
	payload := []byte("unpinned import")
	chunks, data := chunkData(payload)
	bundle := buildBundle(t, len(payload), chunks, data)
	rec := postBundle(t, h, bundle, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	cid := decodeObjectResponse(t, rec).CID

	req := httptest.NewRequest(http.MethodPost, "/v1/gc", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gc status = %d", rec.Code)
	}
	var gc struct {
		Objects int `json:"objects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gc); err != nil {
		t.Fatal(err)
	}
	if gc.Objects != 1 {
		t.Fatalf("gc collected %d objects, want 1 (import must not pin)", gc.Objects)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("collected object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBundleImportMediaType(t *testing.T) {
	h := Handler()
	for _, ct := range []string{"application/json", "application/octet-stream", "application/vnd.cidvault.bundle+cbor"} {
		rec := postBundle(t, h, []byte(`{}`), ct)
		if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
			t.Fatalf("content-type %q: status = %d, code = %q", ct, rec.Code, errorCode(t, rec))
		}
	}
	rec := postBundle(t, h, []byte(`{}`), "")
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("missing content-type: status = %d, code = %q", rec.Code, errorCode(t, rec))
	}
	// Parameters on the correct media type are accepted.
	payload := []byte("x")
	chunks, data := chunkData(payload)
	rec = postBundle(t, h, buildBundle(t, len(payload), chunks, data), "application/vnd.cidvault.bundle+json; charset=utf-8")
	if rec.Code != http.StatusCreated {
		t.Fatalf("parameterized media type: status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestBundleImportMethodNotAllowed(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bundles", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("status = %d, Allow = %q, want 405 POST", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestBundleImportInvalidBundle(t *testing.T) {
	payload := []byte("hello bundle")
	chunks, data := chunkData(payload)
	valid := buildBundle(t, len(payload), chunks, data)

	var doc map[string]any
	if err := json.Unmarshal(valid, &doc); err != nil {
		t.Fatal(err)
	}
	encode := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	cases := map[string][]byte{
		"malformed JSON":     []byte(`{"version":1,`),
		"trailing content":   append(append([]byte{}, valid...), ' ', '{', '}'),
		"not an object":      []byte(`[1,2,3]`),
		"missing version":    []byte(`{"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"missing root":       []byte(`{"version":1,"blocks":[]}`),
		"missing blocks":     []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]}}`),
		"missing root field": []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunks":[]},"blocks":[]}`),
		"unknown field":      []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[],"extra":1}`),
		"unknown root field": []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[],"x":1},"blocks":[]}`),
		"duplicate field":    []byte(`{"version":1,"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"duplicate root key": []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"version as string":  []byte(`{"version":"1","root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"size as float":      []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0.5,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"negative size":      []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":-1,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"chunks null":        []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":null},"blocks":[]}`),
		"root cid malformed": []byte(`{"version":1,"root":{"cid":"sha256:xyz","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"chunk cid malformed": func() []byte {
			d := map[string]any{}
			_ = json.Unmarshal(valid, &d)
			d["root"].(map[string]any)["chunks"] = []any{"nope"}
			return encode(d)
		}(),
		"block cid malformed": func() []byte {
			d := map[string]any{}
			_ = json.Unmarshal(valid, &d)
			d["blocks"] = []any{map[string]any{"cid": "sha256:zz", "data": ""}}
			return encode(d)
		}(),
		"block data not base64": func() []byte {
			d := map[string]any{}
			_ = json.Unmarshal(valid, &d)
			d["blocks"] = []any{map[string]any{"cid": chunks[0], "data": "!!!not-base64!!!"}}
			return encode(d)
		}(),
		"block missing data": func() []byte {
			d := map[string]any{}
			_ = json.Unmarshal(valid, &d)
			d["blocks"] = []any{map[string]any{"cid": chunks[0]}}
			return encode(d)
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postBundle(t, h, body, "application/vnd.cidvault.bundle+json")
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_bundle" {
				t.Fatalf("status = %d, code = %q, want 400 invalid_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestBundleImportUnsupported(t *testing.T) {
	h := Handler()
	cid := expectedRootCID(0, nil)
	for name, body := range map[string][]byte{
		"version 2":     []byte(`{"version":2,"root":{"cid":"` + cid + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"version 0":     []byte(`{"version":0,"root":{"cid":"` + cid + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[]}`),
		"bad chunkSize": []byte(`{"version":1,"root":{"cid":"` + cid + `","size":0,"chunkSize":4096,"chunks":[]},"blocks":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			rec := postBundle(t, h, body, "application/vnd.cidvault.bundle+json")
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "unsupported_bundle" {
				t.Fatalf("status = %d, code = %q, want 422 unsupported_bundle", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestBundleImportTooLarge(t *testing.T) {
	h := Handler()
	cid := expectedRootCID(MaxObjectSize+1, nil)
	body := []byte(fmt.Sprintf(`{"version":1,"root":{"cid":%q,"size":%d,"chunkSize":1048576,"chunks":[]},"blocks":[]}`, cid, MaxObjectSize+1))
	rec := postBundle(t, h, body, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("status = %d, code = %q, want 413 payload_too_large", rec.Code, errorCode(t, rec))
	}
}

func TestBundleImportIntegrityFailures(t *testing.T) {
	payload := make([]byte, ChunkSize+10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	chunks, data := chunkData(payload)
	encode := func(mut func(d *map[string]any)) []byte {
		d := map[string]any{}
		_ = json.Unmarshal(buildBundle(t, len(payload), chunks, data), &d)
		mut(&d)
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	blockList := func(d *map[string]any) []any {
		return (*d)["blocks"].([]any)
	}

	cases := map[string][]byte{
		"digest mismatch": encode(func(d *map[string]any) {
			blocks := blockList(d)
			blocks[0].(map[string]any)["data"] = base64.StdEncoding.EncodeToString([]byte("tampered"))
		}),
		"missing block": encode(func(d *map[string]any) {
			(*d)["blocks"] = blockList(d)[:1]
		}),
		"unreferenced block": encode(func(d *map[string]any) {
			extra := []byte("extra")
			sum := sha256.Sum256(extra)
			(*d)["blocks"] = append(blockList(d), map[string]any{
				"cid":  "sha256:" + hex.EncodeToString(sum[:]),
				"data": base64.StdEncoding.EncodeToString(extra),
			})
		}),
		"duplicate block": encode(func(d *map[string]any) {
			(*d)["blocks"] = append(blockList(d), blockList(d)[0])
		}),
		"non-final chunk short": encode(func(d *map[string]any) {
			short := payload[:ChunkSize-1]
			sum := sha256.Sum256(short)
			cid := "sha256:" + hex.EncodeToString(sum[:])
			(*d)["root"].(map[string]any)["chunks"] = []any{cid, chunks[1]}
			(*d)["blocks"] = []any{
				map[string]any{"cid": cid, "data": base64.StdEncoding.EncodeToString(short)},
				blockList(d)[1],
			}
			(*d)["root"].(map[string]any)["cid"] = expectedRootCID(len(payload), []string{cid, chunks[1]})
		}),
		"wrong object size": encode(func(d *map[string]any) {
			(*d)["root"].(map[string]any)["size"] = len(payload) - 1
		}),
		"wrong root cid": encode(func(d *map[string]any) {
			(*d)["root"].(map[string]any)["cid"] = expectedRootCID(0, nil)
		}),
		"empty object with block": encode(func(d *map[string]any) {
			(*d)["root"].(map[string]any)["size"] = 0
			(*d)["root"].(map[string]any)["chunks"] = []any{}
			(*d)["root"].(map[string]any)["cid"] = expectedRootCID(0, nil)
		}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := Handler()
			rec := postBundle(t, h, body, "application/vnd.cidvault.bundle+json")
			if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "integrity_check_failed" {
				t.Fatalf("status = %d, code = %q, want 422 integrity_check_failed", rec.Code, errorCode(t, rec))
			}
		})
	}
}

func TestBundleImportFailureLeavesStoreUntouched(t *testing.T) {
	h := Handler()
	stats := func() statsResponse {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
		var st statsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	before := stats()

	bad := []byte(`{"version":1,"root":{"cid":"` + expectedRootCID(0, nil) + `","size":0,"chunkSize":1048576,"chunks":[]},"blocks":[{"cid":"` + expectedRootCID(1, nil) + `","data":"aGk="}]}`)
	rec := postBundle(t, h, bad, "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if after := stats(); after != before {
		t.Fatalf("stats changed after failed import: %+v -> %+v", before, after)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/blocks/"+expectedRootCID(1, nil), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("orphan block status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBundleImportConcurrentSingleCreate(t *testing.T) {
	h := Handler()
	payload := []byte("concurrent bundle import")
	chunks, data := chunkData(payload)
	bundle := buildBundle(t, len(payload), chunks, data)

	const n = 8
	var wg sync.WaitGroup
	created := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := postBundle(t, h, bundle, "application/vnd.cidvault.bundle+json")
			if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
				t.Errorf("status = %d", rec.Code)
				return
			}
			created[i] = decodeObjectResponse(t, rec).Created
		}(i)
	}
	wg.Wait()
	count := 0
	for _, c := range created {
		if c {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("created count = %d, want exactly 1", count)
	}
}

func TestBundleImportSharesBlocksWithUpload(t *testing.T) {
	h := Handler()
	payload := []byte("shared block payload")
	up := uploadObject(t, h, payload)

	// Importing the same logical object must report created=false.
	chunks, data := chunkData(payload)
	rec := postBundle(t, h, buildBundle(t, len(payload), chunks, data), "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp := decodeObjectResponse(t, rec); resp.Created || resp.CID != up.CID {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// A new object sharing a block reuses the stored block: stats count the
	// shared block once.
	other := append(append([]byte{}, payload...), 'x')
	ochunks, odata := chunkData(other)
	rec = postBundle(t, h, buildBundle(t, len(other), ochunks, odata), "application/vnd.cidvault.bundle+json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Objects != 2 || st.Blocks != 2 {
		t.Fatalf("stats = %+v, want 2 objects and 2 blocks", st)
	}
}
