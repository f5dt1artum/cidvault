package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func getRef(t *testing.T, h http.Handler, name, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/refs/"+name+query, nil))
	return rec
}

func getRefObject(t *testing.T, h http.Handler, name, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/refs/"+name+"/object"+query, nil))
	return rec
}

func decodeRefResponse(t *testing.T, rec *httptest.ResponseRecorder) refResponse {
	t.Helper()
	var resp refResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ref response is not JSON: %v", err)
	}
	return resp
}

func refBody(cid string, expected int) string {
	return fmt.Sprintf(`{"cid":%q,"expectedRevision":%d}`, cid, expected)
}

func TestRefCreateUpdateAndHistory(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("first target"))
	b := upload(t, h, []byte("second target"))

	// No reference yet.
	rec := getRef(t, h, "release", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty ref status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")

	// First publish creates revision 1.
	rec = putRef(t, h, "release", refBody(a.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	first := decodeRefResponse(t, rec)
	if first.Name != "release" || first.Revision != 1 || first.CID != a.CID {
		t.Fatalf("unexpected create response: %+v", first)
	}
	if first.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt not UTC: %v", first.UpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("updatedAt not RFC3339Nano: %v", err)
	}

	// Re-pointing to the same CID still creates a new revision.
	rec = putRef(t, h, "release", refBody(a.CID, 1), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrite status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeRefResponse(t, rec); got.Revision != 2 || got.CID != a.CID {
		t.Fatalf("unexpected rewrite response: %+v", got)
	}

	// A stale expected revision conflicts.
	rec = putRef(t, h, "release", refBody(b.CID, 1), "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale write status = %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "revision_conflict")

	// A write at the current revision succeeds.
	rec = putRef(t, h, "release", refBody(b.CID, 2), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeRefResponse(t, rec); got.Revision != 3 || got.CID != b.CID {
		t.Fatalf("unexpected update response: %+v", got)
	}

	// Current revision is the latest; history stays readable.
	got := decodeRefResponse(t, getRef(t, h, "release", ""))
	if got.Revision != 3 || got.CID != b.CID || got.Name != "release" {
		t.Fatalf("current revision = %+v, want revision 3 at second target", got)
	}
	got = decodeRefResponse(t, getRef(t, h, "release", "?revision=1"))
	if got.Revision != 1 || got.CID != a.CID {
		t.Fatalf("historical revision = %+v, want revision 1 at first target", got)
	}
	if !got.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("revision 1 updatedAt changed: %v vs %v", got.UpdatedAt, first.UpdatedAt)
	}

	// A revision that does not exist.
	rec = getRef(t, h, "release", "?revision=4")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing revision status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_revision_not_found")

	// Names are independent.
	rec = getRef(t, h, "other", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other name status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")
}

func TestRefObject(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("alpha body"))
	b := upload(t, h, []byte("beta body!"))

	rec := putRef(t, h, "latest", refBody(a.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	rec = putRef(t, h, "latest", refBody(b.CID, 1), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d", rec.Code)
	}

	// Current revision serves the second object.
	rec = getRefObject(t, h, "latest", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("object status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "beta body!" {
		t.Fatalf("object body = %q, want %q", body, "beta body!")
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len("beta body!")) {
		t.Fatalf("Content-Length = %q, want %d", cl, len("beta body!"))
	}
	if cid := rec.Header().Get("X-CidVault-CID"); cid != b.CID {
		t.Fatalf("X-CidVault-CID = %q, want %q", cid, b.CID)
	}
	if rev := rec.Header().Get("X-CidVault-Ref-Revision"); rev != "2" {
		t.Fatalf("X-CidVault-Ref-Revision = %q, want %q", rev, "2")
	}

	// A historical revision serves the first object.
	rec = getRefObject(t, h, "latest", "?revision=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("historical object status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "alpha body" {
		t.Fatalf("historical object body = %q, want %q", body, "alpha body")
	}
	if cid := rec.Header().Get("X-CidVault-CID"); cid != a.CID {
		t.Fatalf("historical X-CidVault-CID = %q, want %q", cid, a.CID)
	}
	if rev := rec.Header().Get("X-CidVault-Ref-Revision"); rev != "1" {
		t.Fatalf("historical X-CidVault-Ref-Revision = %q, want %q", rev, "1")
	}

	// Missing name and missing revision.
	rec = getRefObject(t, h, "absent", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing name status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")
	rec = getRefObject(t, h, "latest", "?revision=3")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing revision status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_revision_not_found")
}

func TestRefSurvivesTargetCollection(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("doomed target"))
	rec := putRef(t, h, "weak", refBody(obj.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// A reference is not a pin: the sweep collects the target.
	if code, result := runGC(t, h, ""); code != http.StatusOK || result.Objects != 1 {
		t.Fatalf("gc = %d %+v, want one collected object", code, result)
	}

	// The reference itself stays readable.
	rec = getRef(t, h, "weak", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ref after gc status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeRefResponse(t, rec); got.Revision != 1 || got.CID != obj.CID {
		t.Fatalf("ref after gc = %+v, want revision 1 at collected cid", got)
	}

	// The object entry reports the missing target.
	rec = getRefObject(t, h, "weak", "")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("object after gc status = %d, want %d", rec.Code, http.StatusFailedDependency)
	}
	assertErrorCode(t, rec, "reference_target_missing")

	// Re-uploading the target makes the object entry work again.
	again := upload(t, h, []byte("doomed target"))
	if !again.Created || again.CID != obj.CID {
		t.Fatalf("re-upload = %+v", again)
	}
	rec = getRefObject(t, h, "weak", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "doomed target" {
		t.Fatalf("object after re-upload = %d %q", rec.Code, rec.Body.String())
	}
}

func TestRefConcurrentWriters(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("race ref"))

	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := putRef(t, h, "race", refBody(obj.CID, 0), "application/json")
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		}
	}
	if created != 1 || conflicts != writers-1 {
		t.Fatalf("concurrent first writes: %d created, %d conflicts, want 1 and %d", created, conflicts, writers-1)
	}
	if got := decodeRefResponse(t, getRef(t, h, "race", "")); got.Revision != 1 {
		t.Fatalf("revision after race = %d, want 1", got.Revision)
	}
}

func TestRefNameValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("named"))

	// Illegal names on every entry point.
	for _, name := range []string{
		"Release",                // uppercase
		".hidden",                // leading dot
		"-dash",                  // leading dash
		"_under",                 // leading underscore
		"a%20b",                  // encoded space decodes to a space
		"a%2Fb",                  // encoded slash decodes to a slash
		"a+b",                    // plus
		strings.Repeat("a", 129), // too long
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/refs/"+name, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s name %q: status = %d, want %d", method, name, rec.Code, http.StatusBadRequest)
			}
			assertErrorCode(t, rec, "invalid_ref")
		}
		rec := getRefObject(t, h, name, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("object name %q: status = %d, want %d", name, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_ref")
	}

	// Boundary names are accepted: single character, 128 characters, every
	// legal symbol.
	for _, name := range []string{"a", "0", strings.Repeat("z", 128), "a.b-c_d"} {
		rec := putRef(t, h, name, refBody(obj.CID, 0), "application/json")
		if rec.Code != http.StatusCreated {
			t.Fatalf("name %q: status = %d, want %d (%s)", name, rec.Code, http.StatusCreated, rec.Body.String())
		}
	}
}

func TestRefRequestValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("validate ref"))

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putRef(t, h, "checked", `{}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Structural errors: not one strict JSON document, missing, unknown,
	// duplicate or mistyped fields, bad expectedRevision.
	for _, body := range []string{
		`{`,
		``,
		`{} {}`,
		`[1]`,
		`{"cid":"x"}`,
		`{"expectedRevision":0}`,
		`{}`,
		`{"cid":"x","expectedRevision":0,"extra":1}`,
		`{"cid":"x","cid":"x","expectedRevision":0}`,
		`{"cid":5,"expectedRevision":0}`,
		`{"cid":null,"expectedRevision":0}`,
		`{"cid":"x","expectedRevision":"0"}`,
		`{"cid":"x","expectedRevision":-1}`,
		`{"cid":"x","expectedRevision":1.5}`,
		`{"cid":"x","expectedRevision":null}`,
	} {
		rec := putRef(t, h, "checked", body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// A string cid with an illegal shape.
	for _, cid := range []string{"", "x", "sha256:zz", "SHA256:" + strings.Repeat("0", 64)} {
		rec := putRef(t, h, "checked", refBody(cid, 0), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cid %q: status = %d, want %d", cid, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}

	// A well-formed cid whose object does not exist.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec := putRef(t, h, "checked", refBody(missing, 0), "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Failures produce no revision: the name is still unpublished.
	rec = getRef(t, h, "checked", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ref after failures status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "ref_not_found")
	rec = putRef(t, h, "checked", refBody(obj.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first write after failures status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestRefQueryErrors(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("query checks"))
	rec := putRef(t, h, "queried", refBody(obj.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	for _, query := range []string{
		"?revision=0", "?revision=-1", "?revision=abc", "?revision=1.0", "?revision=",
		"?revision=1&revision=2", "?unknown=1", "?revision=1&unknown=2",
	} {
		rec := getRef(t, h, "queried", query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("ref query %q: status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
		rec = getRefObject(t, h, "queried", query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("object query %q: status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestRefSideEffects(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("no ref side effects"))
	before := listPins(t, h)
	rec := putRef(t, h, "quiet", refBody(obj.CID, 0), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// No pin is created.
	if after := listPins(t, h); len(after.Pins) != len(before.Pins) {
		t.Fatalf("ref created a pin: %+v", after.Pins)
	}

	// No audit event is recorded: the only events are the upload itself.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))
	var audit auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("audit response is not JSON: %v", err)
	}
	for _, ev := range audit.Events {
		if ev.Action != auditObjectUpload {
			t.Fatalf("unexpected audit event after ref write: %+v", ev)
		}
	}

	// Storage statistics are unchanged by references.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var stats struct {
		Objects      int `json:"objects"`
		LogicalBytes int `json:"logicalBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if stats.Objects != 1 || stats.LogicalBytes != len("no ref side effects") {
		t.Fatalf("stats changed by ref: %+v", stats)
	}
}

func TestRefMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/refs/some", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /v1/refs/some: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
			t.Fatalf("%s /v1/refs/some: Allow = %q, want %q", method, allow, "GET, PUT")
		}
		assertErrorCode(t, rec, "method_not_allowed")

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/refs/some/object", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /v1/refs/some/object: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Fatalf("%s /v1/refs/some/object: Allow = %q, want %q", method, allow, "GET")
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}
