package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

func putRecursivePin(t *testing.T, h http.Handler, cid, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/recursive-pins/"+cid, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getRecursivePin(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/recursive-pins/"+cid, nil))
	return rec
}

func deleteRecursivePin(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/recursive-pins/"+cid, nil))
	return rec
}

func decodeRecursivePinResponse(t *testing.T, rec *httptest.ResponseRecorder) recursivePinResponse {
	t.Helper()
	var resp recursivePinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("recursive pin response is not JSON: %v", err)
	}
	return resp
}

// sortedCopy returns a sorted copy of cids for member-list comparisons.
func sortedCopy(cids ...string) []string {
	out := append([]string(nil), cids...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRecursivePinCreateGetDelete(t *testing.T) {
	h := Handler()
	rootCID, subCID, fileCID := buildTree(t, h)
	rootBody := marshalDirectory(t, []dirEntrySpec{
		{name: "sub", typ: "directory", cid: subCID},
		{name: "top.txt", typ: "file", cid: fileCID},
	})
	subBody := marshalDirectory(t, []dirEntrySpec{
		{name: "inner.txt", typ: "file", cid: fileCID},
	})
	wantBytes := len(rootBody) + len(subBody) + len("file-bytes")
	wantMembers := sortedCopy(rootCID, subCID, fileCID)

	// Create a permanent recursive pin.
	rec := putRecursivePin(t, h, rootCID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.CID != rootCID || resp.ExpiresAt != nil {
		t.Fatalf("unexpected create response: %+v", resp)
	}
	if resp.Objects != 3 || resp.Bytes != wantBytes {
		t.Fatalf("objects/bytes = %d/%d, want 3/%d", resp.Objects, resp.Bytes, wantBytes)
	}
	if !equalStrings(resp.Members, wantMembers) {
		t.Fatalf("members = %v, want %v", resp.Members, wantMembers)
	}

	// GET returns the same snapshot.
	rec = getRecursivePin(t, h, rootCID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeRecursivePinResponse(t, rec); !equalStrings(got.Members, wantMembers) ||
		got.Objects != 3 || got.Bytes != wantBytes || got.ExpiresAt != nil {
		t.Fatalf("unexpected get response: %+v", got)
	}

	// Recursive pins and their members are not listed by GET /v1/pins.
	if list := listPins(t, h); len(list.Pins) != 0 {
		t.Fatalf("recursive pin must not appear in /v1/pins: %+v", list.Pins)
	}

	// Updating an in-force record returns 200 and the new expiration.
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = putRecursivePin(t, h, rootCID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp = decodeRecursivePinResponse(t, rec)
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(time.Now().Add(time.Hour).UTC().Truncate(time.Second)) {
		t.Fatalf("unexpected updated expiry: %+v", resp.ExpiresAt)
	}

	// Delete, then delete and get again.
	if rec = deleteRecursivePin(t, h, rootCID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		deleteRecursivePin(t, h, rootCID),
		getRecursivePin(t, h, rootCID),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("after delete status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "recursive_pin_not_found")
	}
}

func TestRecursivePinDeduplicatesSharedMembers(t *testing.T) {
	h := Handler()
	// The file and the subdirectory are each referenced twice; the root
	// references itself as well. Every unique object counts once.
	fileCID := uploadBytes(t, h, []byte("shared-file"))
	subCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "a.txt", typ: "file", cid: fileCID},
		{name: "b.txt", typ: "file", cid: fileCID},
	}))
	subBody := marshalDirectory(t, []dirEntrySpec{
		{name: "a.txt", typ: "file", cid: fileCID},
		{name: "b.txt", typ: "file", cid: fileCID},
	})
	rootCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "again", typ: "directory", cid: subCID},
		{name: "f.txt", typ: "file", cid: fileCID},
		{name: "sub", typ: "directory", cid: subCID},
	}))
	rootBody := marshalDirectory(t, []dirEntrySpec{
		{name: "again", typ: "directory", cid: subCID},
		{name: "f.txt", typ: "file", cid: fileCID},
		{name: "sub", typ: "directory", cid: subCID},
	})

	rec := putRecursivePin(t, h, rootCID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.Objects != 3 {
		t.Fatalf("objects = %d, want 3 (dedup): %+v", resp.Objects, resp)
	}
	if resp.Bytes != len(rootBody)+len(subBody)+len("shared-file") {
		t.Fatalf("bytes = %d, want %d", resp.Bytes, len(rootBody)+len(subBody)+len("shared-file"))
	}
	if !equalStrings(resp.Members, sortedCopy(rootCID, subCID, fileCID)) {
		t.Fatalf("members = %v", resp.Members)
	}
}

func TestRecursivePinValidationErrors(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putRecursivePin(t, h, rootCID, `{}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Not a single strict JSON document, unknown or duplicate fields, wrong
	// types, trailing data.
	for _, body := range []string{
		``,
		`{`,
		`{"expiresAt":`,
		`{"unknown":1}`,
		`{"expiresAt":123}`,
		`{"expiresAt":null,"expiresAt":null}`,
		`{} {}`,
		`[1]`,
	} {
		rec := putRecursivePin(t, h, rootCID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// Malformed or non-future expirations.
	for _, body := range []string{
		`{"expiresAt":"not-a-time"}`,
		`{"expiresAt":"2020-01-01T00:00:00Z"}`,
		fmt.Sprintf(`{"expiresAt":%q}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
	} {
		rec := putRecursivePin(t, h, rootCID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_expiration")
	}

	// Invalid cid shape on all three methods.
	for _, rec := range []*httptest.ResponseRecorder{
		putRecursivePin(t, h, "not-a-cid", `{}`, "application/json"),
		getRecursivePin(t, h, "not-a-cid"),
		deleteRecursivePin(t, h, "not-a-cid"),
	} {
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid cid status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}

	// Valid cid, missing root object.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec := putRecursivePin(t, h, missing, `{}`, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing root status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Reading or deleting a record that never existed.
	for _, rec := range []*httptest.ResponseRecorder{
		getRecursivePin(t, h, missing),
		deleteRecursivePin(t, h, missing),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing record status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "recursive_pin_not_found")
	}

	// The root exists but is not a directory document.
	plainCID := uploadBytes(t, h, []byte("not a directory"))
	rec = putRecursivePin(t, h, plainCID, `{}`, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-directory root status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")

	// A referenced child directory is not a directory document.
	badChildRoot := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "child", typ: "directory", cid: plainCID},
	}))
	rec = putRecursivePin(t, h, badChildRoot, `{}`, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid child directory status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")

	// A referenced object is absent.
	absentRoot := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "gone", typ: "file", cid: missing},
	}))
	rec = putRecursivePin(t, h, absentRoot, `{}`, "application/json")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("missing target status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "recursive_pin_target_missing")

	// Failures leave no record behind.
	for _, cid := range []string{plainCID, badChildRoot, absentRoot} {
		if rec := getRecursivePin(t, h, cid); rec.Code != http.StatusNotFound {
			t.Fatalf("failed pin of %s left a record: status = %d", cid, rec.Code)
		}
	}
}

func TestRecursivePinEmptyDirectory(t *testing.T) {
	h := Handler()
	rootCID := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{}))
	rec := putRecursivePin(t, h, rootCID, `{"expiresAt":null}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.Objects != 1 || !equalStrings(resp.Members, []string{rootCID}) {
		t.Fatalf("empty directory should pin only the root: %+v", resp)
	}
}

func TestExpiredRecursivePinBehavesAsAbsent(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)

	expiry := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	rec := putRecursivePin(t, h, rootCID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	if rec = getRecursivePin(t, h, rootCID); rec.Code != http.StatusOK {
		t.Fatalf("record should be readable before expiry: status = %d", rec.Code)
	}

	time.Sleep(300 * time.Millisecond)

	for _, rec := range []*httptest.ResponseRecorder{
		getRecursivePin(t, h, rootCID),
		deleteRecursivePin(t, h, rootCID),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expired record status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "recursive_pin_not_found")
	}
	// Re-pinning after expiry is a creation, not an update.
	if rec = putRecursivePin(t, h, rootCID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("re-pin after expiry status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestRecursivePinProtectsMembersFromGC(t *testing.T) {
	h := Handler()
	rootCID, subCID, fileCID := buildTree(t, h)
	other := upload(t, h, []byte("unrelated"))

	rec := putRecursivePin(t, h, rootCID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// Dry run previews only the unprotected object.
	code, preview := runGC(t, h, "?dryRun=true")
	if code != http.StatusOK || preview.Objects != 1 || len(preview.CIDs) != 1 || preview.CIDs[0] != other.CID {
		t.Fatalf("unexpected dry run: code=%d %+v", code, preview)
	}

	// The sweep collects only the unprotected object; members survive.
	if _, result := runGC(t, h, ""); result.Objects != 1 || result.CIDs[0] != other.CID {
		t.Fatalf("unexpected gc result: %+v", result)
	}
	for _, cid := range []string{rootCID, subCID, fileCID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("member %s collected, GET status = %d", cid, rec.Code)
		}
	}

	// Deleting the record does not delete the objects by itself...
	if rec := deleteRecursivePin(t, h, rootCID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	for _, cid := range []string{rootCID, subCID, fileCID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("delete removed member %s, GET status = %d", cid, rec.Code)
		}
	}
	// ...but the next sweep collects them all.
	_, result := runGC(t, h, "")
	if result.Objects != 3 {
		t.Fatalf("gc after delete should collect the 3 members: %+v", result)
	}
}

func TestRecursivePinAndDirectPinOverlap(t *testing.T) {
	h := Handler()
	rootCID, subCID, fileCID := buildTree(t, h)

	// A direct pin on one member and a recursive pin on the root overlap.
	if rec := putPin(t, h, fileCID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("direct pin status = %d", rec.Code)
	}
	if rec := putRecursivePin(t, h, rootCID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("recursive pin status = %d", rec.Code)
	}
	// Removing the recursive record must not release the directly pinned
	// member, while the other members become collectable.
	if rec := deleteRecursivePin(t, h, rootCID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	_, result := runGC(t, h, "")
	if result.Objects != 2 {
		t.Fatalf("gc should collect root and subdir only: %+v", result)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+fileCID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("directly pinned member collected, GET status = %d", rec.Code)
	}
	for _, cid := range []string{rootCID, subCID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unprotected member %s survived, GET status = %d", cid, rec.Code)
		}
	}
}

func TestRecursivePinRecordsOverlap(t *testing.T) {
	h := Handler()
	// Two recursive pins on different roots share a subtree.
	fileCID := uploadBytes(t, h, []byte("shared"))
	sharedSub := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "f", typ: "file", cid: fileCID},
	}))
	rootA := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "shared", typ: "directory", cid: sharedSub},
	}))
	rootB := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
		{name: "shared", typ: "directory", cid: sharedSub},
		{name: "unique-b", typ: "file", cid: fileCID},
	}))
	if rec := putRecursivePin(t, h, rootA, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin A status = %d", rec.Code)
	}
	if rec := putRecursivePin(t, h, rootB, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin B status = %d", rec.Code)
	}
	// Deleting one record keeps the shared subtree protected by the other.
	if rec := deleteRecursivePin(t, h, rootA); rec.Code != http.StatusNoContent {
		t.Fatalf("delete A status = %d", rec.Code)
	}
	_, result := runGC(t, h, "")
	if result.Objects != 1 || result.CIDs[0] != rootA {
		t.Fatalf("gc should collect only root A: %+v", result)
	}
	for _, cid := range []string{rootB, sharedSub, fileCID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("member %s protected by record B collected, GET status = %d", cid, rec.Code)
		}
	}
}

func TestRecursivePinDepthLimit(t *testing.T) {
	h := Handler()
	// A chain of directories: depth of the last node equals its distance
	// from the root. 65 levels (root at depth 0, leaf at depth 64) is
	// accepted; 66 levels is rejected.
	build := func(levels int) string {
		cid := uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{}))
		for i := 1; i < levels; i++ {
			cid = uploadBytes(t, h, marshalDirectory(t, []dirEntrySpec{
				{name: "down", typ: "directory", cid: cid},
			}))
		}
		return cid
	}
	deepest := build(maxRecursivePinDepth + 2) // leaf would sit at depth 65
	rec := putRecursivePin(t, h, deepest, `{}`, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too deep status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "recursive_pin_too_large")
	if rec := getRecursivePin(t, h, deepest); rec.Code != http.StatusNotFound {
		t.Fatalf("rejected deep pin left a record: status = %d", rec.Code)
	}

	accepted := build(maxRecursivePinDepth + 1) // leaf at depth 64
	rec = putRecursivePin(t, h, accepted, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("depth-64 chain should be accepted: status = %d: %s", rec.Code, rec.Body.String())
	}
	if resp := decodeRecursivePinResponse(t, rec); resp.Objects != maxRecursivePinDepth+1 {
		t.Fatalf("objects = %d, want %d", resp.Objects, maxRecursivePinDepth+1)
	}
}

func TestRecursivePinMemberLimit(t *testing.T) {
	s := newStore()
	// Root references three subdirectories, each carrying 4096 file
	// entries: 1 + 3 + 3*4096 = 12292 unique members, over the limit.
	mkDir := func(entries []dirEntrySpec) string {
		obj, _ := s.put(newObject(marshalDirectory(t, entries)), auditObjectUpload, nil)
		return obj.cid
	}
	var subCIDs []string
	for d := 0; d < 3; d++ {
		entries := []dirEntrySpec{}
		for i := 0; i < maxDirectoryEntries; i++ {
			body := []byte(fmt.Sprintf("file-%d-%d", d, i))
			obj, _ := s.put(newObject(body), auditObjectUpload, nil)
			entries = append(entries, dirEntrySpec{name: fmt.Sprintf("f%04d", i), typ: "file", cid: obj.cid})
		}
		subCIDs = append(subCIDs, mkDir(entries))
	}
	rootEntries := []dirEntrySpec{}
	for i, c := range subCIDs {
		rootEntries = append(rootEntries, dirEntrySpec{name: fmt.Sprintf("d%d", i), typ: "directory", cid: c})
	}
	rootCID := mkDir(rootEntries)

	_, _, code := s.setRecursivePin(rootCID, nil, time.Now())
	if code != "recursive_pin_too_large" {
		t.Fatalf("code = %q, want recursive_pin_too_large", code)
	}
	if _, ok := s.recursivePinAt(rootCID, time.Now()); ok {
		t.Fatal("rejected oversized pin left a record")
	}
}

func TestRecursivePinMethodNotAllowed(t *testing.T) {
	h := Handler()
	cid := "sha256:" + strings.Repeat("0", 64)
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodHead, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/recursive-pins/"+cid, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT, DELETE" {
			t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, PUT, DELETE")
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestRecursivePinWritesNoAudit(t *testing.T) {
	h := Handler()
	rootCID, _, _ := buildTree(t, h)
	countEvents := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))
		var resp auditResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("audit response is not JSON: %v", err)
		}
		return len(resp.Events)
	}
	before := countEvents()
	if rec := putRecursivePin(t, h, rootCID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	if rec := deleteRecursivePin(t, h, rootCID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if after := countEvents(); after != before {
		t.Fatalf("recursive pin operations wrote audit events: before=%d after=%d", before, after)
	}
}
