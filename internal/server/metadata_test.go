package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func putMetadata(t *testing.T, h http.Handler, cid, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/objects/"+cid+"/metadata", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getMetadata(t *testing.T, h http.Handler, cid, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid+"/metadata"+query, nil))
	return rec
}

func decodeMetadataResponse(t *testing.T, rec *httptest.ResponseRecorder) metadataResponse {
	t.Helper()
	var resp metadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("metadata response is not JSON: %v", err)
	}
	return resp
}

func TestMetadataCreateUpdateAndHistory(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("metadata me"))

	// No metadata yet.
	rec := getMetadata(t, h, obj.CID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty metadata status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "metadata_not_found")

	// First write creates revision 1.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{"b":"2","a":"1"}}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	first := decodeMetadataResponse(t, rec)
	if first.CID != obj.CID || first.Revision != 1 || first.ContentType == nil || *first.ContentType != "text/plain" {
		t.Fatalf("unexpected create response: %+v", first)
	}
	if len(first.Labels) != 2 || first.Labels["a"] != "1" || first.Labels["b"] != "2" {
		t.Fatalf("unexpected labels: %+v", first.Labels)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("updatedAt not RFC3339Nano: %v", err)
	}
	if first.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt not UTC: %v", first.UpdatedAt)
	}
	// Labels encode sorted by key.
	if body := rec.Body.String(); strings.Index(body, `"a"`) > strings.Index(body, `"b"`) {
		t.Fatalf("labels not sorted by key: %s", body)
	}

	// Rewriting identical content still creates a new revision.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":"text/plain","labels":{"b":"2","a":"1"}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrite status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeMetadataResponse(t, rec); got.Revision != 2 {
		t.Fatalf("rewrite revision = %d, want 2", got.Revision)
	}

	// A stale expected revision conflicts.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale write status = %d, want %d", rec.Code, http.StatusConflict)
	}
	assertErrorCode(t, rec, "revision_conflict")

	// A changed write at the current revision succeeds.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":2,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	third := decodeMetadataResponse(t, rec)
	if third.Revision != 3 || third.ContentType != nil || len(third.Labels) != 0 {
		t.Fatalf("unexpected update response: %+v", third)
	}
	if !strings.Contains(rec.Body.String(), `"labels":{}`) {
		t.Fatalf("empty labels should encode as {}: %s", rec.Body.String())
	}

	// Current revision is the latest; history stays readable.
	got := decodeMetadataResponse(t, getMetadata(t, h, obj.CID, ""))
	if got.Revision != 3 || got.ContentType != nil {
		t.Fatalf("current revision = %+v, want revision 3 with null contentType", got)
	}
	got = decodeMetadataResponse(t, getMetadata(t, h, obj.CID, "?revision=1"))
	if got.Revision != 1 || got.ContentType == nil || *got.ContentType != "text/plain" || got.Labels["a"] != "1" {
		t.Fatalf("historical revision = %+v, want revision 1 content", got)
	}
	if !got.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("revision 1 updatedAt changed: %v vs %v", got.UpdatedAt, first.UpdatedAt)
	}

	// A revision that does not exist.
	rec = getMetadata(t, h, obj.CID, "?revision=4")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing revision status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "metadata_revision_not_found")
}

func TestMetadataConcurrentWriters(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("race metadata"))

	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
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
	if got := decodeMetadataResponse(t, getMetadata(t, h, obj.CID, "")); got.Revision != 1 {
		t.Fatalf("revision after race = %d, want 1", got.Revision)
	}
}

func TestMetadataRequestValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("validate metadata"))

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putMetadata(t, h, obj.CID, `{}`, ct)
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
		`{"expectedRevision":0,"contentType":null}`,
		`{"expectedRevision":0,"labels":{}}`,
		`{"contentType":null,"labels":{}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{},"extra":1}`,
		`{"expectedRevision":0,"expectedRevision":0,"contentType":null,"labels":{}}`,
		`{"expectedRevision":"0","contentType":null,"labels":{}}`,
		`{"expectedRevision":-1,"contentType":null,"labels":{}}`,
		`{"expectedRevision":1.5,"contentType":null,"labels":{}}`,
		`{"expectedRevision":null,"contentType":null,"labels":{}}`,
		`{"expectedRevision":0,"contentType":5,"labels":{}}`,
		`{"expectedRevision":0,"contentType":null,"labels":null}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"a":1}}`,
	} {
		rec := putMetadata(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// Value violations.
	longContentType := strings.Repeat("a", 256)
	longValue := strings.Repeat("v", 257)
	manyLabels := make(map[string]string, 65)
	for i := range 65 {
		manyLabels[fmt.Sprintf("k%02d", i)] = "v"
	}
	manyLabelsJSON, _ := json.Marshal(map[string]any{
		"expectedRevision": 0, "contentType": nil, "labels": manyLabels,
	})
	invalid := []string{
		`{"expectedRevision":0,"contentType":"","labels":{}}`,
		`{"expectedRevision":0,"contentType":"` + longContentType + `","labels":{}}`,
		`{"expectedRevision":0,"contentType":"a\nb","labels":{}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"":"v"}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"A":"v"}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"-a":"v"}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"` + strings.Repeat("k", 65) + `":"v"}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"a":"` + longValue + `"}}`,
		`{"expectedRevision":0,"contentType":null,"labels":{"a":"\t"}}`,
		string(manyLabelsJSON),
	}
	for _, body := range invalid {
		rec := putMetadata(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_metadata")
	}

	// Boundary values are accepted: 255-code-point contentType, 64 labels,
	// 64-character key, 256-code-point value, Unicode content.
	labels := map[string]string{strings.Repeat("k", 64): strings.Repeat("界", 256)}
	for i := range 63 {
		labels[fmt.Sprintf("l%02d", i)] = ""
	}
	okBody, _ := json.Marshal(map[string]any{
		"expectedRevision": 0,
		"contentType":      strings.Repeat("界", 255),
		"labels":           labels,
	})
	rec := putMetadata(t, h, obj.CID, string(okBody), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("boundary body: status = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Failures produce no revision: the first successful write above is
	// still revision 1, and the next write must expect exactly it.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("write after failures status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMetadataPathAndQueryErrors(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("path checks"))

	// Invalid cid shape on both methods.
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/objects/not-a-cid/metadata", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s invalid cid: status = %d, want %d", method, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}

	// Valid cid, missing object.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec := getMetadata(t, h, missing, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
	rec = putMetadata(t, h, missing, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("put missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Query parameter errors on GET.
	for _, query := range []string{
		"?revision=0", "?revision=-1", "?revision=abc", "?revision=1.0", "?revision=",
		"?revision=1&revision=2", "?unknown=1", "?revision=1&unknown=2",
	} {
		rec := getMetadata(t, h, obj.CID, query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestMetadataGarbageCollection(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("collect my metadata"))
	rec := putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{}}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// A dry run changes nothing.
	if code, _ := runGC(t, h, "?dryRun=true"); code != http.StatusOK {
		t.Fatalf("dry run status = %d", code)
	}
	if rec := getMetadata(t, h, obj.CID, ""); rec.Code != http.StatusOK {
		t.Fatalf("metadata after dry run status = %d, want %d", rec.Code, http.StatusOK)
	}

	// A real sweep deletes the history atomically with the object.
	if code, result := runGC(t, h, ""); code != http.StatusOK || result.Objects != 1 {
		t.Fatalf("gc = %d %+v, want one collected object", code, result)
	}
	rec = getMetadata(t, h, obj.CID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("metadata after gc status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Re-uploading the same content restores the object but not the
	// history: metadata starts over at revision 1.
	again := upload(t, h, []byte("collect my metadata"))
	if !again.Created || again.CID != obj.CID {
		t.Fatalf("re-upload = %+v", again)
	}
	rec = getMetadata(t, h, obj.CID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("metadata after re-upload status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "metadata_not_found")
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first write after re-upload status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := decodeMetadataResponse(t, rec); got.Revision != 1 {
		t.Fatalf("revision after re-upload = %d, want 1", got.Revision)
	}
}

func TestMetadataSideEffects(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("no side effects"))
	before := listPins(t, h)
	rec := putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{"a":"1"}}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}

	// No pin is created.
	if after := listPins(t, h); len(after.Pins) != len(before.Pins) {
		t.Fatalf("metadata created a pin: %+v", after.Pins)
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
			t.Fatalf("unexpected audit event after metadata write: %+v", ev)
		}
	}

	// Storage statistics are unchanged by metadata.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var stats struct {
		Objects      int `json:"objects"`
		LogicalBytes int `json:"logicalBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if stats.Objects != 1 || stats.LogicalBytes != len("no side effects") {
		t.Fatalf("stats changed by metadata: %+v", stats)
	}
}

func TestMetadataMethodNotAllowed(t *testing.T) {
	h := Handler()
	cid := "sha256:" + strings.Repeat("0", 64)
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/objects/"+cid+"/metadata", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
			t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, PUT")
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}
