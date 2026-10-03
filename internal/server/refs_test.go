package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// putRef issues a PUT /v1/refs/{name} with the given body and media type.
func putRef(t *testing.T, h http.Handler, name, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/refs/"+name, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getRef issues a GET against a refs route; target is the path after
// "/v1/refs" including any query string.
func getRef(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/refs"+target, nil))
	return rec
}

// refRaw issues a refs request with a verbatim request target, used for
// targets net/url would reject such as malformed percent escapes.
func refRaw(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, &http.Request{Method: method, RequestURI: target})
	return rec
}

func decodeRefResponse(t *testing.T, rec *httptest.ResponseRecorder) refResponse {
	t.Helper()
	var resp refResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ref response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func TestRefCreateUpdateAndHistory(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("ref alpha"))
	b := upload(t, h, []byte("ref beta"))

	// Unknown name.
	rec := getRef(h, "/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing ref status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")

	// First write creates revision 1.
	rec = putRef(t, h, "alpha", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, a.CID), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	first := decodeRefResponse(t, rec)
	if first.Name != "alpha" || first.Revision != 1 || first.CID != a.CID {
		t.Fatalf("unexpected create response: %+v", first)
	}
	if first.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt not UTC: %v", first.UpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("updatedAt not RFC3339Nano: %v", err)
	}

	// The current revision reads back identically.
	got := decodeRefResponse(t, getRef(h, "/alpha"))
	if got != first {
		t.Fatalf("current revision = %+v, want %+v", got, first)
	}

	// Re-pointing at the same cid still creates a new revision.
	rec = putRef(t, h, "alpha", fmt.Sprintf(`{"cid":%q,"expectedRevision":1}`, a.CID), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("same-cid rewrite status = %d, want %d", rec.Code, http.StatusOK)
	}
	second := decodeRefResponse(t, rec)
	if second.Revision != 2 || second.CID != a.CID {
		t.Fatalf("same-cid rewrite = %+v, want revision 2", second)
	}

	// A stale expected revision conflicts.
	rec = putRef(t, h, "alpha", fmt.Sprintf(`{"cid":%q,"expectedRevision":1}`, a.CID), "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale write status = %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "revision_conflict")

	// Point at the second object.
	rec = putRef(t, h, "alpha", fmt.Sprintf(`{"cid":%q,"expectedRevision":2}`, b.CID), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("retarget status = %d, want %d", rec.Code, http.StatusOK)
	}
	third := decodeRefResponse(t, rec)
	if third.Revision != 3 || third.CID != b.CID {
		t.Fatalf("retarget = %+v, want revision 3 at beta", third)
	}

	// History stays readable per revision, with the original timestamps.
	for i, want := range []refResponse{first, second, third} {
		got := decodeRefResponse(t, getRef(h, fmt.Sprintf("/alpha?revision=%d", i+1)))
		if got.Name != want.Name || got.Revision != want.Revision || got.CID != want.CID ||
			!got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Fatalf("revision %d = %+v, want %+v", i+1, got, want)
		}
	}

	// A historical revision that does not exist.
	rec = getRef(h, "/alpha?revision=4")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing revision status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_revision_not_found")
}

func TestRefConcurrentWriters(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("race ref"))
	body := fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, obj.CID)

	const writers = 8
	race := func(expected string) []int {
		codes := make([]int, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b := strings.Replace(body, `"expectedRevision":0`, `"expectedRevision":`+expected, 1)
				codes[i] = putRef(t, h, "raced", b, "application/json").Code
			}()
		}
		wg.Wait()
		return codes
	}

	// First writes: exactly one creates.
	codes := race("0")
	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 || conflicts != writers-1 {
		t.Fatalf("concurrent first writes: %d created, %d conflicts, want 1 and %d", created, conflicts, writers-1)
	}

	// Same-expected updates on revision 1: exactly one succeeds.
	codes = race("1")
	ok, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 1 || conflicts != writers-1 {
		t.Fatalf("concurrent updates: %d ok, %d conflicts, want 1 and %d", ok, conflicts, writers-1)
	}
	if got := decodeRefResponse(t, getRef(h, "/raced")); got.Revision != 2 {
		t.Fatalf("revision after races = %d, want 2", got.Revision)
	}
}

func TestRefObjectServesRawBytes(t *testing.T) {
	h := Handler()
	payload := append(bytes.Repeat([]byte("0123456789ABCDEF"), ChunkSize/16), []byte("tail-bytes")...)
	a := upload(t, h, payload)
	b := upload(t, h, []byte("second target"))

	if rec := putRef(t, h, "file.bin", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, a.CID), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}

	// Current target: raw bytes, exact length, identifying headers.
	rec := getRef(h, "/file.bin/object")
	if rec.Code != http.StatusOK {
		t.Fatalf("object status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != string(payload) {
		t.Fatalf("object body mismatch: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(payload))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cid := rec.Header().Get("X-CidVault-CID"); cid != a.CID {
		t.Fatalf("X-CidVault-CID = %q, want %s", cid, a.CID)
	}
	if rev := rec.Header().Get("X-CidVault-Ref-Revision"); rev != "1" {
		t.Fatalf("X-CidVault-Ref-Revision = %q, want 1", rev)
	}

	// Move the ref to b; current object follows.
	if rec := putRef(t, h, "file.bin", fmt.Sprintf(`{"cid":%q,"expectedRevision":1}`, b.CID), "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("retarget status = %d", rec.Code)
	}
	rec = getRef(h, "/file.bin/object")
	if rec.Code != http.StatusOK || rec.Body.String() != "second target" {
		t.Fatalf("current object = %d %q, want beta bytes", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-CidVault-CID") != b.CID || rec.Header().Get("X-CidVault-Ref-Revision") != "2" {
		t.Fatalf("current object headers = CID %q rev %q, want %s/2", rec.Header().Get("X-CidVault-CID"), rec.Header().Get("X-CidVault-Ref-Revision"), b.CID)
	}

	// A historical revision still serves the old target with its headers.
	rec = getRef(h, "/file.bin/object?revision=1")
	if rec.Code != http.StatusOK || rec.Body.String() != string(payload) {
		t.Fatalf("historical object = %d, want revision 1 bytes", rec.Code)
	}
	if rec.Header().Get("X-CidVault-CID") != a.CID || rec.Header().Get("X-CidVault-Ref-Revision") != "1" {
		t.Fatalf("historical object headers = CID %q rev %q, want %s/1", rec.Header().Get("X-CidVault-CID"), rec.Header().Get("X-CidVault-Ref-Revision"), a.CID)
	}

	// Unknown revision and unknown name on the object endpoint.
	rec = getRef(h, "/file.bin/object?revision=9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("future revision object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_revision_not_found")
	rec = getRef(h, "/nope/object")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown name object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")
}

func TestRefSurvivesGarbageCollection(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("collected target a"))
	b := upload(t, h, []byte("collected target b"))
	putRef(t, h, "hist", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, a.CID), "application/json")
	putRef(t, h, "hist", fmt.Sprintf(`{"cid":%q,"expectedRevision":1}`, b.CID), "application/json")

	// A dry run changes nothing.
	if code, _ := runGC(t, h, "?dryRun=true"); code != http.StatusOK {
		t.Fatalf("dry run status = %d", code)
	}
	if rec := getRef(h, "/hist/object?revision=1"); rec.Code != http.StatusOK {
		t.Fatalf("object after dry run status = %d", rec.Code)
	}

	// Real sweep collects both unpinned objects; refs create no pin.
	if code, result := runGC(t, h, ""); code != http.StatusOK || result.Objects != 2 {
		t.Fatalf("gc = %d %+v, want two collected objects", code, result)
	}

	// Ref metadata, current and historical, stays readable.
	if rec := getRef(h, "/hist"); rec.Code != http.StatusOK {
		t.Fatalf("current ref after gc status = %d, want %d", rec.Code, http.StatusOK)
	} else if got := decodeRefResponse(t, rec); got.Revision != 2 || got.CID != b.CID {
		t.Fatalf("current ref after gc = %+v", got)
	}
	if rec := getRef(h, "/hist?revision=1"); rec.Code != http.StatusOK {
		t.Fatalf("historical ref after gc status = %d", rec.Code)
	} else if got := decodeRefResponse(t, rec); got.CID != a.CID {
		t.Fatalf("historical ref after gc = %+v, want alpha cid", got)
	}

	// Every object entry is a fixed 424 regardless of revision.
	for _, target := range []string{"/hist/object", "/hist/object?revision=1", "/hist/object?revision=2"} {
		rec := getRef(h, target)
		if rec.Code != http.StatusFailedDependency {
			t.Fatalf("%s after gc status = %d, want %d", target, rec.Code, http.StatusFailedDependency)
		}
		assertErrorCode(t, rec, "reference_target_missing")
	}

	// Re-uploading identical content restores the object behind the ref.
	again := upload(t, h, []byte("collected target a"))
	if !again.Created || again.CID != a.CID {
		t.Fatalf("re-upload = %+v", again)
	}
	rec := getRef(h, "/hist/object?revision=1")
	if rec.Code != http.StatusOK || rec.Body.String() != "collected target a" {
		t.Fatalf("object after re-upload = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-CidVault-CID") != a.CID || rec.Header().Get("X-CidVault-Ref-Revision") != "1" {
		t.Fatalf("restored object headers = %q %q", rec.Header().Get("X-CidVault-CID"), rec.Header().Get("X-CidVault-Ref-Revision"))
	}
}

func TestRefPartiallyCollectedHistory(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("unpinned old target"))
	b := upload(t, h, []byte("pinned current target"))
	putRef(t, h, "move", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, a.CID), "application/json")
	putRef(t, h, "move", fmt.Sprintf(`{"cid":%q,"expectedRevision":1}`, b.CID), "application/json")
	// Pin only the current target.
	if rec := putPin(t, h, b.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d", rec.Code)
	}
	if code, result := runGC(t, h, ""); code != http.StatusOK || result.Objects != 1 {
		t.Fatalf("gc = %d %+v, want one collected object", code, result)
	}

	// Current object is served; the collected historical revision is 424.
	if rec := getRef(h, "/move/object"); rec.Code != http.StatusOK || rec.Body.String() != "pinned current target" {
		t.Fatalf("current object = %d %q", rec.Code, rec.Body.String())
	}
	rec := getRef(h, "/move/object?revision=1")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("collected historical object status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "reference_target_missing")
}

func TestRefNoSideEffects(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("no ref side effects"))

	statsBefore := storageStatsSnapshot(t, h)
	pinsBefore := listPins(t, h)
	if rec := putRef(t, h, "side-effect-free", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, obj.CID), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	if got := storageStatsSnapshot(t, h); got != statsBefore {
		t.Fatalf("storage stats changed by ref: before %+v after %+v", statsBefore, got)
	}
	if after := listPins(t, h); len(after.Pins) != len(pinsBefore.Pins) {
		t.Fatalf("ref created a pin: %+v", after.Pins)
	}

	// No audit event names a ref action.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/events?limit=1000", nil))
	var audit auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("audit response is not JSON: %v", err)
	}
	for _, ev := range audit.Events {
		if ev.Action != auditObjectUpload {
			t.Fatalf("unexpected audit event after ref write: %+v", ev)
		}
	}

	// The ref does not protect its target: a sweep still collects it.
	if code, result := runGC(t, h, ""); code != http.StatusOK || result.Objects != 1 {
		t.Fatalf("gc with ref = %d %+v, want the object collected", code, result)
	}
}

func storageStatsSnapshot(t *testing.T, h http.Handler) statsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	return st
}

func TestRefPutValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("validate ref"))

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putRef(t, h, "v", `{"cid":"sha256:`+strings.Repeat("a", 64)+`","expectedRevision":0}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Structural, field and type errors.
	for _, body := range []string{
		`{`,
		``,
		`{} {}`,
		`[1]`,
		`{}`,
		`{"cid":"x"}`,
		`{"expectedRevision":0}`,
		fmt.Sprintf(`{"cid":%q,"expectedRevision":0,"extra":1}`, obj.CID),
		fmt.Sprintf(`{"cid":%q,"cid":%q,"expectedRevision":0}`, obj.CID, obj.CID),
		`{"cid":5,"expectedRevision":0}`,
		`{"cid":null,"expectedRevision":0}`,
		`{"cid":[],"expectedRevision":0}`,
		fmt.Sprintf(`{"cid":%q,"expectedRevision":"0"}`, obj.CID),
		fmt.Sprintf(`{"cid":%q,"expectedRevision":null}`, obj.CID),
		fmt.Sprintf(`{"cid":%q,"expectedRevision":1.5}`, obj.CID),
		fmt.Sprintf(`{"cid":%q,"expectedRevision":-1}`, obj.CID),
		fmt.Sprintf(`{"cid":%q,"expectedRevision":true}`, obj.CID),
	} {
		rec := putRef(t, h, "v", body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// A string cid of the wrong shape.
	for _, cid := range []string{`"not-a-cid"`, `"sha256:short"`, `"sha256:` + strings.Repeat("A", 64) + `"`, `""`} {
		rec := putRef(t, h, "v", `{"cid":`+cid+`,"expectedRevision":0}`, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cid %s: status = %d, want %d", cid, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}

	// Well-formed cid, no such object.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec := putRef(t, h, "v", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, missing), "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing target status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// A non-zero expected revision against a name that does not exist yet.
	rec = putRef(t, h, "other", fmt.Sprintf(`{"cid":%q,"expectedRevision":5}`, obj.CID), "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("future expected revision status = %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "revision_conflict")
}

func TestRefNameValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("name checks"))
	goodBody := fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, obj.CID)

	invalid := []string{
		"", "A", "ABC", "Alpha", "-lead", ".lead", "_lead",
		"a/b", "has space", "café", "a#b",
		strings.Repeat("a", 129),
	}
	for _, name := range invalid {
		esc := url.PathEscape(name)
		if rec := putRef(t, h, esc, goodBody, "application/json"); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT name %q: status = %d, want %d", name, rec.Code, http.StatusBadRequest)
		} else {
			assertErrorCode(t, rec, "invalid_ref")
		}
		if rec := getRef(h, "/"+esc); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET name %q: status = %d, want %d", name, rec.Code, http.StatusBadRequest)
		} else {
			assertErrorCode(t, rec, "invalid_ref")
		}
		if rec := getRef(h, "/"+esc+"/object"); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET object name %q: status = %d, want %d", name, rec.Code, http.StatusBadRequest)
		} else {
			assertErrorCode(t, rec, "invalid_ref")
		}
	}

	// Boundary and character-class names are accepted.
	longest := strings.Repeat("a", 128)
	for _, name := range []string{"a", "0", "a-", "a.", "a_", "a.b_c-1", "9z._-", longest} {
		if rec := putRef(t, h, name, goodBody, "application/json"); rec.Code != http.StatusCreated {
			t.Fatalf("PUT valid name %q: status = %d, want %d: %s", name, rec.Code, http.StatusCreated, rec.Body.String())
		}
	}

	// A percent-encoded legal name decodes exactly once and matches.
	rec := putRef(t, h, "%65ncoded", goodBody, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("encoded valid name status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if got := decodeRefResponse(t, rec); got.Name != "encoded" {
		t.Fatalf("decoded name = %q, want encoded", got.Name)
	}
	if rec := getRef(h, "/%65ncoded"); rec.Code != http.StatusOK {
		t.Fatalf("GET encoded name status = %d, want %d", rec.Code, http.StatusOK)
	}

	// An encoded slash cannot escape into another route; an encoded capital
	// decodes to an illegal name; malformed escapes are rejected.
	for _, target := range []string{
		"/v1/refs/a%2Fb", "/v1/refs/a%2Fb/object",
		"/v1/refs/%41", "/v1/refs/%41/object",
	} {
		rec := getRef(h, strings.TrimPrefix(target, "/v1/refs"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_ref")
	}
	rec = refRaw(h, http.MethodGet, "/v1/refs/%zz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed escape status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_ref")
}

func TestRefQueryErrors(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("query checks"))
	putRef(t, h, "q", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, obj.CID), "application/json")

	bad := []string{
		"/q?revision=0", "/q?revision=-1", "/q?revision=abc", "/q?revision=1.0",
		"/q?revision=", "/q?revision=1&revision=2", "/q?unknown=1", "/q?revision=1&unknown=2",
		"/q/object?revision=0", "/q/object?revision=x", "/q/object?revision=1&revision=2",
		"/q/object?other=1",
	}
	for _, target := range bad {
		rec := getRef(h, target)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestRefMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/refs/name", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /v1/refs/name: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
			t.Fatalf("%s Allow = %q, want %q", method, allow, "GET, PUT")
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/refs/name/object", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /object: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s Allow = %q, want %q", method, allow, http.MethodGet)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestRefDispatchEdgeCases(t *testing.T) {
	h := Handler()
	// More segments than the two known shapes, or a fixed unknown segment,
	// are not refs routes.
	for _, target := range []string{"/v1/refs/a/other", "/v1/refs/a/object/extra", "/v1/refs", "/v1/refs/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 404/400", target, rec.Code)
		}
	}
	// Method routing takes precedence over name validation, as elsewhere.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/refs/BAD", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE invalid name status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestRefObjectReadsConsistentWhileCollected(t *testing.T) {
	h := Handler()
	payload := bytes.Repeat([]byte("concurrent-ref-"), 10000)
	obj := upload(t, h, payload)
	putRef(t, h, "live", fmt.Sprintf(`{"cid":%q,"expectedRevision":0}`, obj.CID), "application/json")

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errCh := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := getRef(h, "/live/object")
				switch rec.Code {
				case http.StatusOK:
					if rec.Body.Len() != len(payload) || rec.Body.String() != string(payload) {
						errCh <- fmt.Errorf("partial or mismatched object body during gc: %d bytes", rec.Body.Len())
						return
					}
					if rec.Header().Get("X-CidVault-CID") != obj.CID || rec.Header().Get("X-CidVault-Ref-Revision") != "1" {
						errCh <- fmt.Errorf("inconsistent headers during gc: %q %q", rec.Header().Get("X-CidVault-CID"), rec.Header().Get("X-CidVault-Ref-Revision"))
						return
					}
				case http.StatusFailedDependency:
					// The target was collected; ref metadata must remain.
				default:
					errCh <- fmt.Errorf("unexpected status %d during gc: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}
	// Give readers a brief head start, then collect the unpinned target.
	time.Sleep(2 * time.Millisecond)
	if code, _ := runGC(t, h, ""); code != http.StatusOK {
		t.Fatalf("gc status = %d", code)
	}
	time.Sleep(2 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// The ref metadata itself remains readable throughout and after.
	if rec := getRef(h, "/live"); rec.Code != http.StatusOK {
		t.Fatalf("ref metadata after concurrent gc status = %d", rec.Code)
	}
}
