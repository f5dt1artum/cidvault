package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDirEntry mirrors one entry when building directory documents in
// tests.
type testDirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	CID  string `json:"cid"`
}

func mustMarshalDir(t *testing.T, entries []testDirEntry) []byte {
	t.Helper()
	body, err := json.Marshal(struct {
		Version int            `json:"version"`
		Entries []testDirEntry `json:"entries"`
	}{Version: 1, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// uploadObject is provided by retrievals_test.go and stores raw bytes,
// requiring a 201 Created response.

func gatewayGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestGatewayRootDirectoryServedVerbatim(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("file contents"))
	// Unusual whitespace and key order must be preserved byte-for-byte; the
	// gateway serves the stored directory body, not a re-serialization.
	dirBody := []byte(`{"entries":[{"name":"f","type":"file","cid":"` + file.CID + `"}],"version":1} `)
	dir := uploadObject(t, h, dirBody)

	rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != directoryMediaType {
		t.Fatalf("Content-Type = %q, want %q", ct, directoryMediaType)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(dirBody)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(dirBody))
	}
	if !bytes.Equal(rec.Body.Bytes(), dirBody) {
		t.Fatalf("directory body was re-serialized:\n got %q\nwant %q", rec.Body.Bytes(), dirBody)
	}
}

func TestGatewayResolveFileAndNestedDirectory(t *testing.T) {
	h := Handler()
	leaf := uploadObject(t, h, []byte("leaf bytes"))
	inner := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "leaf.txt", Type: "file", CID: leaf.CID}}))
	root := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "inner", Type: "directory", CID: inner.CID}}))

	// Final file: raw bytes, exact length.
	rec := gatewayGet(t, h, "/v1/gateway/"+root.CID+"/inner/leaf.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len("leaf bytes")) {
		t.Fatalf("Content-Length = %q, want %d", cl, len("leaf bytes"))
	}
	if rec.Body.String() != "leaf bytes" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "leaf bytes")
	}

	// Final directory: stored bytes with the directory media type.
	rec = gatewayGet(t, h, "/v1/gateway/"+root.CID+"/inner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != directoryMediaType {
		t.Fatalf("Content-Type = %q, want %q", ct, directoryMediaType)
	}
}

func TestGatewayLargeFileContentLength(t *testing.T) {
	h := Handler()
	payload := bytes.Repeat([]byte("Z"), ChunkSize+7)
	file := uploadObject(t, h, payload)
	dir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "big", Type: "file", CID: file.CID}}))

	rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/big")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("multi-chunk file bytes differ")
	}
}

func TestGatewayPercentDecoding(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("data"))
	dir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{
		{Name: "a b", Type: "file", CID: file.CID},
		{Name: "中", Type: "file", CID: file.CID},
	}))

	cases := []struct {
		path string
		want int
	}{
		{"/a%20b", http.StatusOK},     // single decode: "a b"
		{"/%E4%B8%AD", http.StatusOK}, // percent-encoded UTF-8 matches literally
		{"/a%20", http.StatusNotFound},
		{"/x%2Fy", http.StatusBadRequest},  // decoded slash
		{"/x%5cy", http.StatusBadRequest},  // decoded backslash
		{"/%2e", http.StatusBadRequest},    // decoded "."
		{"/%2E%2E", http.StatusBadRequest}, // decoded ".."
		{"/%ff", http.StatusBadRequest},    // decoded bytes are not UTF-8
		{"/%252E", http.StatusNotFound},    // decoded once to "%2E", a literal name
		{"/%252F", http.StatusNotFound},    // decoded once to "%2F", no slash
		{"/%00", http.StatusNotFound},      // NUL is a legal segment for matching, but no entry can carry it
		{"/%1f", http.StatusNotFound},      // control character, same rationale
		{"/%7f", http.StatusNotFound},      // DEL, same rationale
		{"/missing", http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+tc.path)
		if rec.Code != tc.want {
			t.Fatalf("GET %s: status = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestGatewayNoUnicodeNormalization(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("e-acute"))
	// Entry name is NFC: U+00E9.
	dir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "é", Type: "file", CID: file.CID}}))

	// NFC encoding matches.
	if rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/%C3%A9"); rec.Code != http.StatusOK {
		t.Fatalf("NFC path status = %d, want %d", rec.Code, http.StatusOK)
	}
	// NFD encoding (U+0065 U+0301) is a different segment and must not match.
	rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/%65%CC%81")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("NFD path status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "path_not_found")
}

func TestGatewayInvalidCID(t *testing.T) {
	h := Handler()
	for _, target := range []string{
		"/v1/gateway/not-a-cid",
		"/v1/gateway/sha256:" + strings.Repeat("A", 64),
		"/v1/gateway/sha256:" + strings.Repeat("0", 63) + "/a/b",
	} {
		rec := gatewayGet(t, h, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}
}

func TestGatewayPathErrors(t *testing.T) {
	h := Handler()
	dir := uploadObject(t, h, mustMarshalDir(t, nil))

	for _, suffix := range []string{
		"/",   // trailing slash: empty segment
		"//x", // empty segment
		"/a//b",
		"/.",
		"/..",
	} {
		rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+suffix)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %q: status = %d, want %d", suffix, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_path")
	}
}

func TestGatewayDepthLimit(t *testing.T) {
	h := Handler()
	cid := "sha256:" + strings.Repeat("a", 64)

	ok64, bad65 := cid, cid
	for i := 0; i < 64; i++ {
		ok64 += "/d"
	}
	for i := 0; i < 65; i++ {
		bad65 += "/d"
	}
	// 64 segments is parsed (root happens to be absent -> object_not_found).
	rec := gatewayGet(t, h, "/v1/gateway/"+ok64)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("64 segments: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
	// 65 segments is rejected before the store is consulted.
	rec = gatewayGet(t, h, "/v1/gateway/"+bad65)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("65 segments: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_path")
}

func TestGatewayResolutionErrors(t *testing.T) {
	h := Handler()

	// Root object missing.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec := gatewayGet(t, h, "/v1/gateway/"+missing)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing root: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Root is a plain file read as a directory.
	plainFile := uploadObject(t, h, []byte("i am a file"))
	rec = gatewayGet(t, h, "/v1/gateway/"+plainFile.CID)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("file root: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")
	// A path through the file root still reports invalid_directory (it is
	// read as a directory first).
	rec = gatewayGet(t, h, "/v1/gateway/"+plainFile.CID+"/anything")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("file root with path: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")

	// Entry does not exist.
	file := uploadObject(t, h, []byte("x"))
	dir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "present", Type: "file", CID: file.CID}}))
	rec = gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/absent")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing entry: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "path_not_found")

	// Traversing through a file entry.
	rec = gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/present/child")
	if rec.Code != http.StatusConflict {
		t.Fatalf("through file: status = %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "not_directory")

	// Entry references a locally absent object.
	absentTarget := "sha256:" + strings.Repeat("b", 64)
	broken := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "gone", Type: "file", CID: absentTarget}}))
	rec = gatewayGet(t, h, "/v1/gateway/"+broken.CID+"/gone")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("missing target: status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "gateway_target_missing")

	// A directory-typed entry descending into a missing object.
	brokenDir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "sub", Type: "directory", CID: absentTarget}}))
	rec = gatewayGet(t, h, "/v1/gateway/"+brokenDir.CID+"/sub/child")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("missing mid directory: status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "gateway_target_missing")

	// A directory-typed entry whose object is present but is not a valid
	// directory document.
	badInner := uploadObject(t, h, []byte("{not json"))
	wrap := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "sub", Type: "directory", CID: badInner.CID}}))
	rec = gatewayGet(t, h, "/v1/gateway/"+wrap.CID+"/sub")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid final directory: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")
	rec = gatewayGet(t, h, "/v1/gateway/"+wrap.CID+"/sub/child")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid mid directory: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")
}

func TestGatewayMalformedDirectoryDocuments(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("payload"))
	goodCID := file.CID

	documents := map[string][]byte{
		"not json":             []byte(`{`),
		"trailing content":     []byte(`{"version":1,"entries":[]}extra`),
		"unknown top field":    []byte(`{"version":1,"entries":[],"extra":1}`),
		"missing version":      []byte(`{"entries":[]}`),
		"missing entries":      []byte(`{"version":1}`),
		"duplicate field":      []byte(`{"version":1,"version":1,"entries":[]}`),
		"version wrong":        []byte(`{"version":2,"entries":[]}`),
		"version string":       []byte(`{"version":"1","entries":[]}`),
		"entries not array":    []byte(`{"version":1,"entries":{}}`),
		"entry missing name":   []byte(`{"version":1,"entries":[{"type":"file","cid":"` + goodCID + `"}]}`),
		"entry null name":      []byte(`{"version":1,"entries":[{"name":null,"type":"file","cid":"` + goodCID + `"}]}`),
		"entry unknown field":  []byte(`{"version":1,"entries":[{"name":"a","type":"file","cid":"` + goodCID + `","x":1}]}`),
		"entry duplicate keys": []byte(`{"version":1,"entries":[{"name":"a","name":"b","type":"file","cid":"` + goodCID + `"}]}`),
		"bad type":             []byte(`{"version":1,"entries":[{"name":"a","type":"symlink","cid":"` + goodCID + `"}]}`),
		"bad cid":              []byte(`{"version":1,"entries":[{"name":"a","type":"file","cid":"nope"}]}`),
		"dot name":             []byte(`{"version":1,"entries":[{"name":".","type":"file","cid":"` + goodCID + `"}]}`),
		"dotdot name":          []byte(`{"version":1,"entries":[{"name":"..","type":"file","cid":"` + goodCID + `"}]}`),
		"slash name":           []byte(`{"version":1,"entries":[{"name":"a/b","type":"file","cid":"` + goodCID + `"}]}`),
		"backslash name":       []byte(`{"version":1,"entries":[{"name":"a\\b","type":"file","cid":"` + goodCID + `"}]}`),
		"empty name":           []byte(`{"version":1,"entries":[{"name":"","type":"file","cid":"` + goodCID + `"}]}`),
		"control in name":      []byte(`{"version":1,"entries":[{"name":"a\tb","type":"file","cid":"` + goodCID + `"}]}`),
		"duplicate names":      []byte(`{"version":1,"entries":[{"name":"a","type":"file","cid":"` + goodCID + `"},{"name":"a","type":"file","cid":"` + goodCID + `"}]}`),
		"unsorted names":       []byte(`{"version":1,"entries":[{"name":"b","type":"file","cid":"` + goodCID + `"},{"name":"a","type":"file","cid":"` + goodCID + `"}]}`),
	}
	for name, body := range documents {
		dir := uploadObject(t, h, body)
		rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want %d", name, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_directory")
	}

	// A name of 256 code points is invalid; 255 is accepted.
	long255 := strings.Repeat("é", 255)
	long256 := strings.Repeat("é", 256)
	for _, tc := range []struct {
		name string
		want int
	}{
		{long255, http.StatusOK},
		{long256, http.StatusUnprocessableEntity},
	} {
		body, err := json.Marshal(struct {
			Version int `json:"version"`
			Entries []testDirEntry
		}{1, []testDirEntry{{Name: tc.name, Type: "file", CID: goodCID}}})
		if err != nil {
			t.Fatal(err)
		}
		dir := uploadObject(t, h, body)
		rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID)
		if rec.Code != tc.want {
			t.Fatalf("name length %d: status = %d, want %d", len([]rune(tc.name)), rec.Code, tc.want)
		}
	}

	// 4096 entries are accepted; 4097 rejected.
	for _, n := range []int{maxDirectoryEntries, maxDirectoryEntries + 1} {
		entries := make([]testDirEntry, n)
		for i := range entries {
			entries[i] = testDirEntry{Name: fmt.Sprintf("e%04d", i), Type: "file", CID: goodCID}
		}
		body := mustMarshalDir(t, entries)
		dir := uploadObject(t, h, body)
		rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID)
		want := http.StatusOK
		if n > maxDirectoryEntries {
			want = http.StatusUnprocessableEntity
		}
		if rec.Code != want {
			t.Fatalf("%d entries: status = %d, want %d", n, rec.Code, want)
		}
	}
}

func TestGatewayMethodNotAllowed(t *testing.T) {
	h := Handler()
	dir := uploadObject(t, h, mustMarshalDir(t, nil))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/gateway/"+dir.CID+"/a", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestGatewayReadsAreNotAudited(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("audited uploads only"))
	dir := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "f", Type: "file", CID: file.CID}}))

	countEvents := func() int {
		rec := gatewayGet(t, h, "/v1/audit/events?limit=1000")
		var resp auditResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return len(resp.Events)
	}
	before := countEvents()
	for range 5 {
		if rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID); rec.Code != http.StatusOK {
			t.Fatalf("dir read status = %d", rec.Code)
		}
		if rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/f"); rec.Code != http.StatusOK {
			t.Fatalf("file read status = %d", rec.Code)
		}
		if rec := gatewayGet(t, h, "/v1/gateway/"+dir.CID+"/missing"); rec.Code != http.StatusNotFound {
			t.Fatalf("missing read status = %d", rec.Code)
		}
	}
	if after := countEvents(); after != before {
		t.Fatalf("audit events changed from %d to %d due to gateway reads", before, after)
	}
}

func TestGatewayDeepChainAtSegmentLimit(t *testing.T) {
	h := Handler()
	file := uploadObject(t, h, []byte("deep leaf"))
	prev := file.CID
	prevType := "file"
	for range 64 {
		entries := []testDirEntry{{Name: "d", Type: prevType, CID: prev}}
		dir := uploadObject(t, h, mustMarshalDir(t, entries))
		prev = dir.CID
		prevType = "directory"
	}
	root := prev

	// 63 directory hops plus the final "d" entry pointing at the file:
	// exactly 64 segments.
	target := "/v1/gateway/" + root + strings.Repeat("/d", 64)
	rec := gatewayGet(t, h, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("64-segment chain: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "deep leaf" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "deep leaf")
	}
}

func TestSnapshotObjectsIsPointInTime(t *testing.T) {
	s := newStore()
	s.audit = newAuditLog()
	obj := newObject([]byte("collected soon"))
	s.put(obj, auditObjectUpload, nil)

	snap := s.snapshotObjects()
	if _, ok := snap[obj.cid]; !ok {
		t.Fatal("snapshot missing the object immediately after put")
	}
	// Garbage collection removes the object and its blocks; the snapshot
	// keeps describing the earlier instant.
	cids, _ := s.collectGarbage(time.Now(), false)
	if len(cids) != 1 {
		t.Fatalf("collected %d objects, want 1", len(cids))
	}
	if got, ok := snap[obj.cid]; !ok || got.size != len("collected soon") {
		t.Fatal("snapshot changed after concurrent garbage collection")
	}
	// The snapshot keeps the object's bytes readable even though the block
	// table released them when the object was collected.
	if data := objectBytes(snap[obj.cid]); string(data) != "collected soon" {
		t.Fatalf("snapshot bytes = %q after collection", data)
	}
	if s.get(obj.cid) != nil {
		t.Fatal("object should be gone from the live store")
	}
}

func TestGatewayConcurrentReadsAndCollection(t *testing.T) {
	h := Handler()

	// A pinned tree survives every collection: its reads must keep
	// succeeding while other requests churn the store.
	file := uploadObject(t, h, []byte("stable"))
	inner := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "f", Type: "file", CID: file.CID}}))
	root := uploadObject(t, h, mustMarshalDir(t, []testDirEntry{{Name: "inner", Type: "directory", CID: inner.CID}}))
	for _, c := range []string{file.CID, inner.CID, root.CID} {
		body := `{"expiresAt":null}`
		req := httptest.NewRequest(http.MethodPut, "/v1/pins/"+c, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("pin %s status = %d", c, rec.Code)
		}
	}

	const readers, iterations = 8, 100
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				target := "/v1/gateway/" + root.CID
				if j%2 == 0 {
					target += "/inner/f"
				}
				rec := gatewayGet(t, h, target)
				if rec.Code != http.StatusOK {
					t.Errorf("reader %d iteration %d: status = %d, want 200", i, j, rec.Code)
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := range iterations {
			// Upload fresh (unique) unpinned junk, then collect it at once.
			uploadObject(t, h, []byte(fmt.Sprintf("junk-%d", j)))
			req := httptest.NewRequest(http.MethodPost, "/v1/gc", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("gc status = %d", rec.Code)
				return
			}
		}
	}()
	wg.Wait()

	// The pinned tree is still served byte-identically afterwards.
	rec := gatewayGet(t, h, "/v1/gateway/"+root.CID+"/inner/f")
	if rec.Code != http.StatusOK || rec.Body.String() != "stable" {
		t.Fatalf("tree after churn: status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestParseGatewayPath(t *testing.T) {
	cidOK := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		rest     string
		wantCID  bool
		segments int
		code     string
	}{
		{cidOK, true, 0, ""},
		{cidOK + "/a", true, 1, ""},
		{cidOK + "/a/b/c", true, 3, ""},
		{cidOK + "/a%20b", true, 1, ""},
		{cidOK + "/a%zz", false, 0, "invalid_path"},
		{cidOK + "/%ff", false, 0, "invalid_path"},
		{cidOK + "/a%2Fb", false, 0, "invalid_path"},
		{cidOK + "/a/", false, 0, "invalid_path"},
		{cidOK + "//a", false, 0, "invalid_path"},
		{cidOK + "/%2e", false, 0, "invalid_path"},
		{cidOK + "/" + strings.Repeat("d/", 63) + "d", true, 64, ""},
		{cidOK + "/" + strings.Repeat("d/", 64) + "d", false, 0, "invalid_path"},
		{"not-a-cid", false, 0, "invalid_cid"},
		{"sha256%3A" + strings.Repeat("a", 64), true, 0, ""},
	}
	for _, tc := range cases {
		cid, segments, code := parseGatewayPath(tc.rest)
		if code != tc.code {
			t.Errorf("parseGatewayPath(%q): code = %q, want %q", tc.rest, code, tc.code)
		}
		if code == "" {
			if tc.wantCID && !validCID(cid) {
				t.Errorf("parseGatewayPath(%q): bad cid %q", tc.rest, cid)
			}
			if len(segments) != tc.segments {
				t.Errorf("parseGatewayPath(%q): %d segments, want %d", tc.rest, len(segments), tc.segments)
			}
		}
	}
}

func TestParseDirectoryDocumentUnit(t *testing.T) {
	goodCID := "sha256:" + strings.Repeat("1", 64)
	if doc, ok := parseDirectoryDocument(nil); ok || doc != nil {
		t.Fatal("empty body should not be a directory")
	}
	good := mustMarshalDir(t, []testDirEntry{{Name: "a", Type: "file", CID: goodCID}})
	doc, ok := parseDirectoryDocument(good)
	if !ok || doc.Version != 1 || len(doc.Entries) != 1 {
		t.Fatalf("valid document rejected: doc=%v ok=%v", doc, ok)
	}
}
