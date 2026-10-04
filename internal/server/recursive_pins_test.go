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

// dirEnt describes one directory entry for uploadDir.
type dirEnt struct {
	name string
	typ  string
	cid  string
}

// dirBody builds a valid directory document from entries, sorting them by
// name as the format requires.
func dirBody(entries ...dirEnt) []byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var b strings.Builder
	b.WriteString(`{"version":1,"entries":[`)
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":%q,"type":%q,"cid":%q}`, e.name, e.typ, e.cid)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// uploadDir stores a directory object with the given entries.
func uploadDir(t *testing.T, h http.Handler, entries ...dirEnt) objectResponse {
	t.Helper()
	return upload(t, h, dirBody(entries...))
}

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

// buildTree stores file-a and file-b under directory sub, and sub plus a
// third file under the root directory. It returns the root directory
// response, the sub directory response and the three file responses in
// upload order.
func buildPinTree(t *testing.T, h http.Handler) (root, sub objectResponse, files []objectResponse) {
	t.Helper()
	files = []objectResponse{
		upload(t, h, []byte("alpha file")),
		upload(t, h, []byte("beta file")),
		upload(t, h, []byte("gamma file")),
	}
	sub = uploadDir(t, h,
		dirEnt{"a.txt", "file", files[0].CID},
		dirEnt{"b.txt", "file", files[1].CID},
	)
	root = uploadDir(t, h,
		dirEnt{"c.txt", "file", files[2].CID},
		dirEnt{"sub", "directory", sub.CID},
	)
	return root, sub, files
}

func TestRecursivePinCreateGetUpdateDelete(t *testing.T) {
	h := Handler()
	root, sub, files := buildPinTree(t, h)

	// Create a permanent recursive pin over the tree.
	rec := putRecursivePin(t, h, root.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.CID != root.CID || resp.ExpiresAt != nil {
		t.Fatalf("unexpected create response: %+v", resp)
	}
	wantMembers := []string{root.CID, sub.CID, files[0].CID, files[1].CID, files[2].CID}
	sort.Strings(wantMembers)
	if resp.Objects != 5 {
		t.Fatalf("objects = %d, want 5", resp.Objects)
	}
	wantBytes := root.Size + sub.Size + len("alpha file") + len("beta file") + len("gamma file")
	if resp.Bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d", resp.Bytes, wantBytes)
	}
	if !sort.StringsAreSorted(resp.Members) {
		t.Fatalf("members not sorted: %+v", resp.Members)
	}
	if strings.Join(resp.Members, ",") != strings.Join(wantMembers, ",") {
		t.Fatalf("members = %v, want %v", resp.Members, wantMembers)
	}

	// GET returns the record with the commit-time member snapshot.
	rec = getRecursivePin(t, h, root.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", rec.Code, http.StatusOK)
	}
	got := decodeRecursivePinResponse(t, rec)
	if got.Objects != resp.Objects || got.Bytes != resp.Bytes ||
		strings.Join(got.Members, ",") != strings.Join(wantMembers, ",") || got.ExpiresAt != nil {
		t.Fatalf("unexpected get response: %+v", got)
	}

	// Updating an in-force record returns 200 and replaces the expiration.
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = putRecursivePin(t, h, root.CID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp = decodeRecursivePinResponse(t, rec)
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(time.Now().Add(time.Hour).UTC().Truncate(time.Second)) {
		t.Fatalf("unexpected updated expiry: %+v", resp.ExpiresAt)
	}

	// Delete, then delete and get again.
	if rec := deleteRecursivePin(t, h, root.CID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = deleteRecursivePin(t, h, root.CID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "recursive_pin_not_found")
	rec = getRecursivePin(t, h, root.CID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "recursive_pin_not_found")
}

func TestRecursivePinNullAndEmptyBodyArePermanent(t *testing.T) {
	h := Handler()
	root, _, _ := buildPinTree(t, h)

	for _, body := range []string{`{"expiresAt":null}`, ``, `{}`} {
		rec := putRecursivePin(t, h, root.CID, body, "application/json")
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("body %q: status = %d", body, rec.Code)
		}
		if resp := decodeRecursivePinResponse(t, rec); resp.ExpiresAt != nil {
			t.Fatalf("body %q: expiresAt = %+v, want null", body, resp.ExpiresAt)
		}
	}
}

func TestRecursivePinValidationErrors(t *testing.T) {
	h := Handler()
	root, _, _ := buildPinTree(t, h)

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putRecursivePin(t, h, root.CID, `{}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Malformed JSON, unknown fields, duplicate fields, wrong types,
	// trailing data, non-object documents.
	for _, body := range []string{
		`{`,
		`{"expiresAt":`,
		`{"unknown":1}`,
		`{"expiresAt":null,"expiresAt":null}`,
		`{"expiresAt":123}`,
		`{"expiresAt":["x"]}`,
		`{} {}`,
		`[1]`,
		`"x"`,
		`null`,
	} {
		rec := putRecursivePin(t, h, root.CID, body, "application/json")
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
		rec := putRecursivePin(t, h, root.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_expiration")
	}

	// Invalid cid shape on all three methods.
	for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/recursive-pins/not-a-cid", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s invalid cid: status = %d, want %d", method, rec.Code, http.StatusBadRequest)
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

	// Root object that is not a directory document.
	plain := upload(t, h, []byte("not a directory"))
	rec = putRecursivePin(t, h, plain.CID, `{}`, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-directory root status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")

	// Get and delete on a cid with no record.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/recursive-pins/"+missing, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s missing record: status = %d, want %d", method, rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "recursive_pin_not_found")
	}
}

func TestRecursivePinTargetMissing(t *testing.T) {
	h := Handler()
	absent := "sha256:" + strings.Repeat("1", 64)

	// A file entry pointing at an absent object fails the whole pin.
	dir := uploadDir(t, h, dirEnt{"gone.txt", "file", absent})
	rec := putRecursivePin(t, h, dir.CID, `{}`, "application/json")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("missing file target status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "recursive_pin_target_missing")

	// A directory entry pointing at an absent object fails the same way.
	dir2 := uploadDir(t, h, dirEnt{"gone", "directory", absent})
	rec = putRecursivePin(t, h, dir2.CID, `{}`, "application/json")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("missing directory target status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "recursive_pin_target_missing")

	// A nested directory whose body is not a directory document is invalid.
	notDir := upload(t, h, []byte("plain bytes"))
	root := uploadDir(t, h, dirEnt{"bad", "directory", notDir.CID})
	rec = putRecursivePin(t, h, root.CID, `{}`, "application/json")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid nested directory status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_directory")

	// Failures leave no record behind.
	for _, cid := range []string{dir.CID, dir2.CID, root.CID} {
		rec := getRecursivePin(t, h, cid)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("get after failed pin: status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "recursive_pin_not_found")
	}
}

func TestRecursivePinDeduplicatesSharedMembers(t *testing.T) {
	h := Handler()
	shared := upload(t, h, []byte("shared file"))
	subA := uploadDir(t, h, dirEnt{"x.txt", "file", shared.CID})
	subB := uploadDir(t, h,
		dirEnt{"again", "directory", subA.CID},
		dirEnt{"y.txt", "file", shared.CID},
	)
	// The root references subA twice (directly and via subB) and the shared
	// file three times; every object is counted once.
	root := uploadDir(t, h,
		dirEnt{"a", "directory", subA.CID},
		dirEnt{"b", "directory", subB.CID},
		dirEnt{"z.txt", "file", shared.CID},
	)

	rec := putRecursivePin(t, h, root.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.Objects != 4 {
		t.Fatalf("objects = %d, want 4 unique members", resp.Objects)
	}
	wantBytes := root.Size + subA.Size + subB.Size + shared.Size
	if resp.Bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d", resp.Bytes, wantBytes)
	}
}

func TestRecursivePinHandlesReferenceCycle(t *testing.T) {
	h := Handler()
	// The cycle is root2 -> child2 -> root -> child; child is an empty
	// directory. The walk terminates because members are de-duplicated, and
	// every object in the cycle is counted exactly once.
	child := uploadDir(t, h)
	root := uploadDir(t, h, dirEnt{"child", "directory", child.CID})
	child2 := uploadDir(t, h, dirEnt{"back", "directory", root.CID})
	root2 := uploadDir(t, h, dirEnt{"child", "directory", child2.CID})

	rec := putRecursivePin(t, h, root2.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeRecursivePinResponse(t, rec)
	if resp.Objects != 4 {
		t.Fatalf("objects = %d, want 4 (cycle visited once)", resp.Objects)
	}
}

func TestRecursivePinProtectsMembersFromGC(t *testing.T) {
	h := Handler()
	root, sub, files := buildPinTree(t, h)
	unrelated := upload(t, h, []byte("unrelated"))

	if rec := putRecursivePin(t, h, root.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// Preview and real sweep collect only the unrelated object.
	code, preview := runGC(t, h, "?dryRun=true")
	if code != http.StatusOK || preview.Objects != 1 || preview.CIDs[0] != unrelated.CID {
		t.Fatalf("unexpected dry run: %+v", preview)
	}
	_, result := runGC(t, h, "")
	if result.Objects != 1 || result.CIDs[0] != unrelated.CID {
		t.Fatalf("unexpected gc: %+v", result)
	}
	for _, cid := range []string{root.CID, sub.CID, files[0].CID, files[1].CID, files[2].CID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("protected object %s collected, GET status = %d", cid, rec.Code)
		}
	}

	// Deleting the record releases the whole tree to the next sweep.
	if rec := deleteRecursivePin(t, h, root.CID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	_, result = runGC(t, h, "")
	if result.Objects != 5 {
		t.Fatalf("gc after delete collected %d objects, want 5", result.Objects)
	}
}

func TestRecursivePinAndDirectPinOverlap(t *testing.T) {
	h := Handler()
	root, _, files := buildPinTree(t, h)

	// A direct pin on one file plus a recursive pin on the root.
	if rec := putPin(t, h, files[0].CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("direct pin status = %d", rec.Code)
	}
	if rec := putRecursivePin(t, h, root.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("recursive pin status = %d", rec.Code)
	}

	// Deleting the recursive record must not release the directly pinned
	// file; everything else in the tree goes.
	if rec := deleteRecursivePin(t, h, root.CID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	_, result := runGC(t, h, "")
	if result.Objects != 4 {
		t.Fatalf("gc collected %d objects, want 4 (direct pin keeps one file)", result.Objects)
	}
	for _, cid := range result.CIDs {
		if cid == files[0].CID {
			t.Fatalf("directly pinned file collected: %+v", result.CIDs)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+files[0].CID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("directly pinned file missing, GET status = %d", rec.Code)
	}
}

func TestOverlappingRecursivePins(t *testing.T) {
	h := Handler()
	shared := upload(t, h, []byte("shared subtree file"))
	sharedDir := uploadDir(t, h, dirEnt{"s.txt", "file", shared.CID})
	rootA := uploadDir(t, h, dirEnt{"shared", "directory", sharedDir.CID})
	rootB := uploadDir(t, h, dirEnt{"shared-too", "directory", sharedDir.CID})

	if rec := putRecursivePin(t, h, rootA.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin A status = %d", rec.Code)
	}
	if rec := putRecursivePin(t, h, rootB.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin B status = %d", rec.Code)
	}

	// Deleting one record keeps the shared subtree protected by the other.
	if rec := deleteRecursivePin(t, h, rootA.CID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete A status = %d", rec.Code)
	}
	_, result := runGC(t, h, "")
	if result.Objects != 1 || result.CIDs[0] != rootA.CID {
		t.Fatalf("gc should collect only root A: %+v", result)
	}
	for _, cid := range []string{rootB.CID, sharedDir.CID, shared.CID} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("object %s protected by remaining record collected", cid)
		}
	}
}

func TestExpiredRecursivePinBehavesAsAbsent(t *testing.T) {
	h := Handler()
	root, _, _ := buildPinTree(t, h)

	expiry := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	rec := putRecursivePin(t, h, root.CID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec := getRecursivePin(t, h, root.CID); rec.Code != http.StatusOK {
		t.Fatalf("get before expiry status = %d, want %d", rec.Code, http.StatusOK)
	}

	time.Sleep(300 * time.Millisecond)

	// Expired records are neither readable nor deletable.
	rec = getRecursivePin(t, h, root.CID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get expired status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "recursive_pin_not_found")
	rec = deleteRecursivePin(t, h, root.CID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete expired status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "recursive_pin_not_found")

	// The expired record no longer protects the tree.
	_, result := runGC(t, h, "")
	if result.Objects != 5 {
		t.Fatalf("gc collected %d objects, want 5 after expiry", result.Objects)
	}

	// Re-pinning after expiry is a creation, not an update. The tree was
	// collected, so rebuild it first.
	root, _, _ = buildPinTree(t, h)
	rec = putRecursivePin(t, h, root.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-pin after expiry status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestRecursivePinNotListedAsDirectPin(t *testing.T) {
	h := Handler()
	root, _, files := buildPinTree(t, h)

	if rec := putRecursivePin(t, h, root.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	if list := listPins(t, h); len(list.Pins) != 0 {
		t.Fatalf("recursive pin members must not appear in /v1/pins: %+v", list.Pins)
	}

	// A direct pin on a member still lists only that member.
	if rec := putPin(t, h, files[0].CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("direct pin status = %d", rec.Code)
	}
	list := listPins(t, h)
	if len(list.Pins) != 1 || list.Pins[0].CID != files[0].CID {
		t.Fatalf("unexpected pins: %+v", list.Pins)
	}
}

func TestRecursivePinWritesNoAuditEvents(t *testing.T) {
	h := Handler()
	root, _, _ := buildPinTree(t, h)

	if rec := putRecursivePin(t, h, root.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	if rec := getRecursivePin(t, h, root.CID); rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if rec := deleteRecursivePin(t, h, root.CID); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("audit response is not JSON: %v", err)
	}
	for _, ev := range resp.Events {
		if ev.Action != auditObjectUpload {
			t.Fatalf("unexpected audit event from recursive pin operations: %+v", ev)
		}
	}
}

func TestRecursivePinMethodNotAllowed(t *testing.T) {
	h := Handler()
	cid := "sha256:" + strings.Repeat("0", 64)
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodHead} {
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

// insertObject stores one object directly in the store, bypassing HTTP.
func insertObject(t *testing.T, s *store, body []byte) *object {
	t.Helper()
	obj, created := s.put(newObject(body), auditObjectUpload, nil)
	if !created {
		t.Fatalf("object for body %q already existed", body)
	}
	return obj
}

// insertDir stores one directory object directly in the store.
func insertDir(t *testing.T, s *store, entries ...dirEnt) *object {
	t.Helper()
	return insertObject(t, s, dirBody(entries...))
}

func TestRecursivePinDepthLimitStore(t *testing.T) {
	// chainStore builds a chain of n directories over one leaf file and
	// returns the root cid. The root sits at depth 0, so the leaf ends up
	// at depth n.
	chainStore := func(n int) (*store, string) {
		s := newStore()
		leaf := insertObject(t, s, []byte("leaf"))
		next, nextType := leaf.cid, "file"
		for i := 0; i < n; i++ {
			dir := insertDir(t, s, dirEnt{"next", nextType, next})
			next, nextType = dir.cid, "directory"
		}
		return s, next
	}

	// Deepest member at depth 64: allowed.
	s, rootCID := chainStore(maxRecursivePinDepth)
	_, _, code, _ := s.setRecursivePin(rootCID, nil, time.Now())
	if code != "" {
		t.Fatalf("depth %d chain failed: %s", maxRecursivePinDepth, code)
	}

	// Deepest member at depth 65: too large.
	s, rootCID = chainStore(maxRecursivePinDepth + 1)
	_, _, code, status := s.setRecursivePin(rootCID, nil, time.Now())
	if code != "recursive_pin_too_large" || status != http.StatusUnprocessableEntity {
		t.Fatalf("depth %d chain: code = %q status = %d, want recursive_pin_too_large/422",
			maxRecursivePinDepth+1, code, status)
	}
}

func TestRecursivePinMemberLimit(t *testing.T) {
	// Root plus three subdirectories; the files are distributed so the
	// unique member count lands exactly on and then above the limit.
	build := func(filesPerDir []int) (*store, string) {
		s := newStore()
		ents := []dirEnt{}
		for d, n := range filesPerDir {
			fileEnts := []dirEnt{}
			for i := 0; i < n; i++ {
				f := insertObject(t, s, []byte(fmt.Sprintf("file-%d-%d", d, i)))
				fileEnts = append(fileEnts, dirEnt{fmt.Sprintf("f%04d", i), "file", f.cid})
			}
			sub := insertDir(t, s, fileEnts...)
			ents = append(ents, dirEnt{fmt.Sprintf("d%d", d), "directory", sub.cid})
		}
		root := insertDir(t, s, ents...)
		return s, root.cid
	}

	// 1 root + 3 subdirs + 9996 files = 10000 members: allowed.
	s, rootCID := build([]int{4096, 4096, 1804})
	rp, _, code, _ := s.setRecursivePin(rootCID, nil, time.Now())
	if code != "" {
		t.Fatalf("10000-member tree failed: %s", code)
	}
	if len(rp.members) != maxRecursivePinMembers {
		t.Fatalf("members = %d, want %d", len(rp.members), maxRecursivePinMembers)
	}

	// One more file pushes the tree over the limit.
	s, rootCID = build([]int{4096, 4096, 1805})
	_, _, code, status := s.setRecursivePin(rootCID, nil, time.Now())
	if code != "recursive_pin_too_large" || status != http.StatusUnprocessableEntity {
		t.Fatalf("10001-member tree: code = %q status = %d, want recursive_pin_too_large/422", code, status)
	}
}
