package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// dirEntrySpec describes one directory entry while building test fixtures.
type dirEntrySpec struct {
	name string
	typ  string
	cid  string
}

// marshalDirectory encodes a canonical, contract-conformant directory
// document with entries sorted strictly by Unicode code point.
func marshalDirectory(t *testing.T, entries []dirEntrySpec) []byte {
	t.Helper()
	sorted := append([]dirEntrySpec(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return runesLess(sorted[i].name, sorted[j].name) })
	seen := map[string]bool{}
	type entryJSON struct {
		Name string `json:"name"`
		Type string `json:"type"`
		CID  string `json:"cid"`
	}
	doc := struct {
		Version int         `json:"version"`
		Entries []entryJSON `json:"entries"`
	}{Version: directoryVersion, Entries: []entryJSON{}}
	for _, e := range sorted {
		if seen[e.name] {
			t.Fatalf("duplicate entry name %q while building fixture", e.name)
		}
		seen[e.name] = true
		doc.Entries = append(doc.Entries, entryJSON{Name: e.name, Type: e.typ, CID: e.cid})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// upload stores arbitrary bytes through POST /v1/objects and returns the CID.
func uploadBytes(t *testing.T, h http.Handler, body []byte) string {
	t.Helper()
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}
	return decodeObjectResponse(t, rec).CID
}

// gatewayRequest issues a gateway GET using the raw target verbatim, so
// percent escapes survive exactly as written.
func gatewayRequest(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// gatewayRequestRaw issues a gateway request whose request target would not
// survive net/url parsing (e.g. an invalid percent escape), setting
// RequestURI directly as a real server would hand it to the handler.
func gatewayRequestRaw(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := &http.Request{Method: method, RequestURI: target}
	h.ServeHTTP(rec, req)
	return rec
}

// buildTree stores a file, a leaf directory referencing it, and a root
// directory referencing both. It returns the CIDs of root, subdir and file.
func buildTree(t *testing.T, h http.Handler) (rootCID, subCID, fileCID string) {
	t.Helper()
	fileCID = uploadBytes(t, h, []byte("file-bytes"))
	subCID = uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "inner.txt", typ: "file", cid: fileCID},
	}))
	rootCID = uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "sub", typ: "directory", cid: subCID},
		{name: "top.txt", typ: "file", cid: fileCID},
	}))
	return rootCID, subCID, fileCID
}

func TestGatewayServesRootDirectoryVerbatim(t *testing.T) {
	h := Handler()
	rootCID, subCID, fileCID := buildTree(t, h)
	rootBody := marshalDirectory(t, []dirEntrySpec{
		{name: "sub", typ: "directory", cid: subCID},
		{name: "top.txt", typ: "file", cid: fileCID},
	})

	rec := gatewayRequest(h, "/v1/gateway/"+rootCID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != directoryMediaType {
		t.Fatalf("Content-Type = %q, want %q", ct, directoryMediaType)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(rootBody)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(rootBody))
	}
	if rec.Body.String() != string(rootBody) {
		t.Fatalf("directory body was reserialized:\n got %s\nwant %s", rec.Body.String(), rootBody)
	}
}

func TestGatewayResolvesFilesAndDirectories(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)

	// File at the top level.
	rec := gatewayRequest(h, "/v1/gateway/"+rootCID+"/top.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("top.txt status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("file Content-Type = %q", ct)
	}
	if rec.Body.String() != "file-bytes" || rec.Header().Get("Content-Length") != "10" {
		t.Fatalf("unexpected file response: len=%d body=%q", rec.Body.Len(), rec.Body.String())
	}

	// Intermediate and final directory.
	for _, target := range []string{
		"/v1/gateway/" + rootCID + "/sub",
		"/v1/gateway/" + rootCID + "/sub/", // trailing slash is an empty segment
	} {
		want := http.StatusOK
		if strings.HasSuffix(target, "/") {
			want = http.StatusBadRequest
		}
		got := gatewayRequest(h, target)
		if got.Code != want {
			t.Fatalf("GET %s: status = %d, want %d", target, got.Code, want)
		}
		if want == http.StatusOK {
			if ct := got.Header().Get("Content-Type"); ct != directoryMediaType {
				t.Fatalf("subdir Content-Type = %q", ct)
			}
			var doc struct {
				Entries []struct {
					Name string `json:"name"`
				} `json:"entries"`
			}
			if err := json.Unmarshal(got.Body.Bytes(), &doc); err != nil || len(doc.Entries) != 1 {
				t.Fatalf("subdir body not the stored directory: %v %s", err, got.Body.String())
			}
		}
	}

	// File nested one directory deeper.
	rec = gatewayRequest(h, "/v1/gateway/"+rootCID+"/sub/inner.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != "file-bytes" {
		t.Fatalf("nested file: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestGatewayPathSingleDecoding(t *testing.T) {
	h := Handler()
	fileCID := uploadBytes(t, h, []byte("x"))

	// Entry names exercising exact, single percent-decoding.
	encodedSlash := "%2F-name" // decoded name literally contains "%2F"
	rootCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "a b", typ: "file", cid: fileCID},
		{name: "café", typ: "file", cid: fileCID},
		{name: encodedSlash, typ: "file", cid: fileCID},
	}))

	ok := func(target string) {
		t.Helper()
		if rec := gatewayRequest(h, target); rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d: %s", target, rec.Code, rec.Body.String())
		}
	}
	ok("/v1/gateway/" + rootCID + "/a%20b")
	ok("/v1/gateway/" + rootCID + "/caf%C3%A9")  // uppercase hex
	ok("/v1/gateway/" + rootCID + "/caf%c3%a9")  // lowercase hex
	ok("/v1/gateway/" + rootCID + "/%252F-name") // double encoding decoded once

	invalid := func(target string) {
		t.Helper()
		if rec := gatewayRequest(h, target); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400", target, rec.Code)
		}
	}
	invalid("/v1/gateway/" + rootCID + "/a%2Fb")  // decoded slash
	invalid("/v1/gateway/" + rootCID + "/a%5Cb")  // decoded backslash
	invalid("/v1/gateway/" + rootCID + "/%2E%2E") // decoded ..
	invalid("/v1/gateway/" + rootCID + "/%2E")    // decoded .
	invalid("/v1/gateway/" + rootCID + "/%ff")    // decoded bytes are not UTF-8
	invalid("/v1/gateway/" + rootCID + "/")       // empty segment
	invalid("/v1/gateway/" + rootCID + "//b")     // empty segment

	// Truncated or non-hex escapes do not survive url.Parse either; a real
	// server rejects them while reading the request line, but a raw target
	// handed to the handler must still be classified as invalid_path.
	for _, bad := range []string{"/%", "/%2", "/%zz", "/a%0"} {
		if rec := gatewayRequestRaw(h, http.MethodGet, "/v1/gateway/"+rootCID+bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("raw %s: status = %d, want 400", bad, rec.Code)
		}
	}

	// A decoded NUL is a legal single-decoded, valid-UTF-8 path segment; the
	// control-character prohibition governs stored entry names, not gateway
	// lookups, so it simply matches nothing.
	if rec := gatewayRequest(h, "/v1/gateway/"+rootCID+"/a%00b"); rec.Code != http.StatusNotFound {
		t.Fatalf("decoded NUL segment: status = %d, want 404", rec.Code)
	}

	// Raw dot-segments are intercepted before ServeMux path cleaning.
	invalid("/v1/gateway/" + rootCID + "/..")
	invalid("/v1/gateway/" + rootCID + "/a/../b")
	invalid("/v1/gateway/" + rootCID + "/./b")

	// No Unicode normalization: NFD spelling of an NFC-stored name misses.
	nfd := "/v1/gateway/" + rootCID + "/cafe%CC%81"
	if rec := gatewayRequest(h, nfd); rec.Code != http.StatusNotFound {
		t.Fatalf("NFD lookup status = %d, want 404", rec.Code)
	}
}

func TestGatewaySegmentDepth(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)

	deep := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "d"
		}
		return "/v1/gateway/" + rootCID + "/" + strings.Join(parts, "/")
	}
	// 64 segments is accepted by path validation; the names are absent, so
	// resolution fails at the first one with path_not_found, not invalid_path.
	if rec := gatewayRequest(h, deep(64)); rec.Code != http.StatusNotFound {
		t.Fatalf("64 segments: status = %d, want 404", rec.Code)
	}
	// 65 segments is rejected before the store is consulted.
	if rec := gatewayRequest(h, deep(65)); rec.Code != http.StatusBadRequest {
		t.Fatalf("65 segments: status = %d, want 400", rec.Code)
	}
}

func TestGatewayErrorCodes(t *testing.T) {
	h := Handler()
	rootCID, _, fileCID := buildTree(t, h)

	check := func(target string, wantStatus int, wantCode string) {
		t.Helper()
		rec := gatewayRequest(h, target)
		if rec.Code != wantStatus {
			t.Fatalf("GET %s: status = %d, want %d (%s)", target, rec.Code, wantStatus, rec.Body.String())
		}
		assertErrorCode(t, rec, wantCode)
	}

	validMissing := "sha256:" + strings.Repeat("0", 64)
	check("/v1/gateway/not-a-cid", http.StatusBadRequest, "invalid_cid")
	check("/v1/gateway/sha256:"+strings.Repeat("A", 64), http.StatusBadRequest, "invalid_cid")
	check("/v1/gateway/"+validMissing, http.StatusNotFound, "object_not_found")
	check("/v1/gateway/"+rootCID+"/missing", http.StatusNotFound, "path_not_found")
	check("/v1/gateway/"+rootCID+"/top.txt/child", http.StatusConflict, "not_directory")
	check("/v1/gateway/"+rootCID+"/sub/inner.txt/child", http.StatusConflict, "not_directory")

	// A root object that is a plain file does not satisfy the directory
	// contract even though it is a perfectly good object.
	check("/v1/gateway/"+fileCID, http.StatusUnprocessableEntity, "invalid_directory")

	// A referenced object absent locally is a failed dependency; the gateway
	// never retrieves it remotely even when a provider is published.
	publishLocalhostProvider(t, h, validMissing)
	missingFileRoot := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "ghost", typ: "file", cid: validMissing},
	}))
	missingDirRoot := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "ghostdir", typ: "directory", cid: validMissing},
	}))
	check("/v1/gateway/"+missingFileRoot+"/ghost", http.StatusFailedDependency, "gateway_target_missing")
	check("/v1/gateway/"+missingDirRoot+"/ghostdir", http.StatusFailedDependency, "gateway_target_missing")

	// An intermediate entry declared as a directory but pointing at an object
	// that is not a valid directory is invalid_directory, not target missing.
	fileAsDir := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "link", typ: "directory", cid: fileCID},
	}))
	check("/v1/gateway/"+fileAsDir+"/link/x", http.StatusUnprocessableEntity, "invalid_directory")
}

func TestGatewayMalformedDirectories(t *testing.T) {
	h := Handler()
	validCIDRef := "sha256:" + strings.Repeat("0", 64)
	entry := func(name, typ, cid string) string {
		return fmt.Sprintf(`{"name":%q,"type":%q,"cid":%q}`, name, typ, cid)
	}
	bodies := []string{
		`{"version":2,"entries":[]}`,
		`{"version":1,"entries":[` + entry("b", "file", validCIDRef) + `,` + entry("a", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("a", "file", validCIDRef) + `,` + entry("a", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("a/b", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("a\\b", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry(".", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("..", "file", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("a", "symlink", validCIDRef) + `]}`,
		`{"version":1,"entries":[` + entry("a", "file", "notacid") + `]}`,
		`{"version":1,"entries":[{"name":"a","type":"file"}]}`,
		`{"version":1,"entries":[],"extra":1}`,
		`{"version":1,"entries":null}`,
		`{"version":"1","entries":[]}`,
		`{"version":1}`,
		`{"entries":[]}`,
		`{"version":1,"entries":[]}x`, // trailing non-whitespace content
		`{"version":1,"entries":[` + entry("a", "file", validCIDRef) + `,]}`,
		`not json at all`,
	}
	for i, body := range bodies {
		cid := uploadBytes(t, h, []byte(body))
		rec := gatewayRequest(h, "/v1/gateway/"+cid)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("malformed directory %d: status = %d, want 422 (%s)", i, rec.Code, body)
		}
		assertErrorCode(t, rec, "invalid_directory")
	}

	// An empty-entries directory is valid and serves its own bytes.
	emptyCID := uploadBytes(t, h, marshalDirectory(t, nil))
	rec := gatewayRequest(h, "/v1/gateway/"+emptyCID)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != directoryMediaType {
		t.Fatalf("empty directory: status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestGatewayMethodNotAllowed(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := gatewayRequestRaw(h, method, "/v1/gateway/"+rootCID)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestGatewayAddsNoAuditEvents(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)

	// Establish the audit frontier after the fixture uploads.
	eventsRec := httptest.NewRecorder()
	h.ServeHTTP(eventsRec, httptest.NewRequest(http.MethodGet, "/v1/audit/events?limit=1000", nil))
	var events auditResponse
	if err := json.Unmarshal(eventsRec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	frontier := events.NextAfter

	// Successful and failing gateway reads must all be audit-silent.
	for _, target := range []string{
		"/v1/gateway/" + rootCID,
		"/v1/gateway/" + rootCID + "/top.txt",
		"/v1/gateway/" + rootCID + "/missing",
		"/v1/gateway/sha256:" + strings.Repeat("0", 64),
	} {
		gatewayRequest(h, target)
	}
	after := httptest.NewRecorder()
	h.ServeHTTP(after, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v1/audit/events?after=%d", frontier), nil))
	var page auditResponse
	if err := json.Unmarshal(after.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.HasMore {
		t.Fatalf("gateway reads produced audit events: %+v", page.Events)
	}
}

func TestGatewaySnapshotAcrossConcurrentGC(t *testing.T) {
	h := Handler()

	const iterations = 50
	var wg sync.WaitGroup
	for iter := 0; iter < iterations; iter++ {
		fileCID := uploadBytes(t, h, []byte("snapshot"))
		rootCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
			{name: "f", typ: "file", cid: fileCID},
		}))
		target := "/v1/gateway/" + rootCID + "/f"

		wg.Add(2)
		var readCode int
		go func() {
			defer wg.Done()
			readCode = gatewayRequest(h, target).Code
		}()
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/gc", nil))
			// The GC endpoint is /v1/gc, served by the mux.
			if rec.Code != http.StatusOK {
				t.Errorf("gc status = %d", rec.Code)
			}
		}()
		wg.Wait()

		// A single acceptance snapshot means the reader either saw the whole
		// tree (200) or nothing reachable yet (root already collected -> 404);
		// it must never observe the root without its referenced file.
		if readCode != http.StatusOK && readCode != http.StatusNotFound {
			t.Fatalf("iteration %d: inconsistent snapshot status %d", iter, readCode)
		}
	}
}

// publishLocalhostProvider registers a valid provider for cid so tests can
// prove the gateway does not perform remote retrieval.
func publishLocalhostProvider(t *testing.T, h http.Handler, cid string) {
	t.Helper()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"addresses":["http://127.0.0.1:1/x"],"expiresAt":%q}`, expires)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/"+cid+"/peer-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("provider publish status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDecodeGatewaySegment(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"plain", "plain", true},
		{"a%20b", "a b", true},     // space
		{"a%2fb", "a/b", true},     // slash decoded literally
		{"a%252Fb", "a%2Fb", true}, // decoded exactly once
		{"a+b", "a+b", true},       // plus is literal, never a space
		{"%C3%A9", "é", true},      // two bytes, one rune
		{"%00", "\x00", true},      // NUL decodes; caller rejects via lookup
		{"%7f", "\x7f", true},      // DEL decodes
		{"%", "", false},           // truncated
		{"%2", "", false},          // truncated mid-byte
		{"a%", "", false},          // trailing percent
		{"%zz", "", false},         // non-hex
		{"%2g", "", false},         // non-hex second digit
		{"%G2", "", false},         // non-hex first digit
		{"%ff", "\xff", true},      // decodes to invalid UTF-8 bytes
		{"", "", true},             // empty stays empty; caller rejects
	}
	for _, tc := range cases {
		got, ok := decodeGatewaySegment(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("decodeGatewaySegment(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidDirectoryNameBoundaries(t *testing.T) {
	multi := strings.Repeat("é", maxNameRunes) // 255 code points, 510 bytes
	longASCII := strings.Repeat("a", maxNameRunes)
	tooLongMulti := strings.Repeat("é", maxNameRunes+1)
	for name, want := range map[string]bool{
		longASCII:                true,
		multi:                    true,
		"é":                      true,
		"a b":                    true,
		"a\tb":                   false, // tab is a control character
		"a\nb":                   false,
		"a\x7fb":                 false, // DEL
		"a\xc2\x85b":             false, // U+0085 (NEL) is C1 control
		"":                       false,
		".":                      false,
		"..":                     false,
		"...":                    true,
		"a/b":                    false,
		"a\\b":                   false,
		tooLongMulti:             false,
		strings.Repeat("a", 256): false,
		"a\x00b":                 false,
	} {
		if got := validDirectoryName(name); got != want {
			t.Errorf("validDirectoryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseDirectoryCodePointOrdering(t *testing.T) {
	ref := "sha256:" + strings.Repeat("0", 64)
	// Byte order and code-point order coincide for valid UTF-8; exercise a
	// multibyte pair plus ASCII to confirm ordering is by rune, not length.
	good := fmt.Sprintf(`{"version":1,"entries":[`+
		`{"name":"A","type":"file","cid":%q},`+
		`{"name":"z","type":"directory","cid":%q},`+
		`{"name":"é","type":"file","cid":%q}]}`, ref, ref, ref)
	entries, ok := parseDirectory([]byte(good))
	if !ok || len(entries) != 3 {
		t.Fatalf("valid code-point-ordered directory rejected: ok=%v n=%d", ok, len(entries))
	}

	// Same names reversed must fail the strictly-increasing requirement.
	bad := fmt.Sprintf(`{"version":1,"entries":[`+
		`{"name":"é","type":"file","cid":%q},`+
		`{"name":"z","type":"directory","cid":%q},`+
		`{"name":"A","type":"file","cid":%q}]}`, ref, ref, ref)
	if _, ok := parseDirectory([]byte(bad)); ok {
		t.Fatal("out-of-order directory accepted")
	}

	// A prefix sorts before the longer string.
	pref := fmt.Sprintf(`{"version":1,"entries":[`+
		`{"name":"ab","type":"file","cid":%q},`+
		`{"name":"abc","type":"file","cid":%q}]}`, ref, ref)
	if _, ok := parseDirectory([]byte(pref)); !ok {
		t.Fatal("prefix-then-longer ordering rejected")
	}

	// Overlong JSON for more than 4096 entries must be rejected.
	var b strings.Builder
	fmt.Fprintf(&b, `{"version":1,"entries":[`)
	for i := 0; i <= maxDirectoryEntries; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":%q,"type":"file","cid":%q}`, fmt.Sprintf("n%05d", i), ref)
	}
	b.WriteString(`]}`)
	if _, ok := parseDirectory([]byte(b.String())); ok {
		t.Fatal("directory with 4097 entries accepted")
	}
}
