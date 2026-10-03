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

func TestMetadataWriteAndReadRevisions(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-alpha"))

	rec := putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{"b":"2","a":"1"}}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first write status = %d, want %d", rec.Code, http.StatusCreated)
	}
	first := decodeMetadataResponse(t, rec)
	if first.CID != obj.CID || first.Revision != 1 {
		t.Fatalf("first response = %+v, want revision 1 for %s", first, obj.CID)
	}
	if first.ContentType == nil || *first.ContentType != "text/plain" {
		t.Fatalf("first contentType = %v, want text/plain", first.ContentType)
	}
	if len(first.Labels) != 2 || first.Labels["a"] != "1" || first.Labels["b"] != "2" {
		t.Fatalf("first labels = %v", first.Labels)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("updatedAt not RFC3339Nano: %v", err)
	}
	if first.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt location = %v, want UTC", first.UpdatedAt.Location())
	}
	// Labels encode sorted by key.
	if got := rec.Body.String(); strings.Index(got, `"a"`) > strings.Index(got, `"b"`) {
		t.Fatalf("labels not sorted by key: %s", got)
	}

	// An unchanged write still appends a revision.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":"text/plain","labels":{"a":"1","b":"2"}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("second write status = %d, want %d", rec.Code, http.StatusOK)
	}
	second := decodeMetadataResponse(t, rec)
	if second.Revision != 2 {
		t.Fatalf("second revision = %d, want 2", second.Revision)
	}

	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":2,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("third write status = %d, want %d", rec.Code, http.StatusOK)
	}
	third := decodeMetadataResponse(t, rec)
	if third.Revision != 3 || third.ContentType != nil || len(third.Labels) != 0 {
		t.Fatalf("third response = %+v", third)
	}
	if strings.Contains(rec.Body.String(), `"labels":null`) {
		t.Fatalf("empty labels must encode as object: %s", rec.Body.String())
	}

	// Current revision and history reads.
	cur := decodeMetadataResponse(t, getMetadata(t, h, obj.CID, ""))
	if cur.Revision != 3 || cur.ContentType != nil {
		t.Fatalf("current = %+v, want revision 3", cur)
	}
	old := decodeMetadataResponse(t, getMetadata(t, h, obj.CID, "?revision=1"))
	if old.Revision != 1 || old.ContentType == nil || *old.ContentType != "text/plain" || old.Labels["a"] != "1" {
		t.Fatalf("revision 1 = %+v", old)
	}
	if !old.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("revision 1 updatedAt changed: %v vs %v", old.UpdatedAt, first.UpdatedAt)
	}
}

func TestMetadataRevisionConflicts(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-conflict"))

	// The first write must name expectedRevision 0.
	rec := putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "revision_conflict" {
		t.Fatalf("first write with N=1: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "revision_conflict" {
		t.Fatalf("stale write: status = %d code = %s", rec.Code, errorCode(t, rec))
	}
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":2,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "revision_conflict" {
		t.Fatalf("future write: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	// Failed writes produce no revisions: the next expected revision is 1.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":1,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("write after conflicts status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMetadataConcurrentWrites(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-race"))

	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/v1/objects/"+obj.CID+"/metadata",
				strings.NewReader(`{"expectedRevision":0,"contentType":null,"labels":{}}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
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
		t.Fatalf("created = %d, conflicts = %d, want 1 and %d", created, conflicts, writers-1)
	}
	cur := decodeMetadataResponse(t, getMetadata(t, h, obj.CID, ""))
	if cur.Revision != 1 {
		t.Fatalf("revision after race = %d, want 1", cur.Revision)
	}
}

func TestMetadataPutValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-validate"))

	// Bad path CID and missing object.
	rec := putMetadata(t, h, "not-a-cid", `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("bad cid: status = %d code = %s", rec.Code, errorCode(t, rec))
	}
	missing := strings.Repeat("0", 64)
	rec = putMetadata(t, h, "sha256:"+missing, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "object_not_found" {
		t.Fatalf("missing object: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	// Media type.
	rec = putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("media type: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	badRequest := []string{
		``,                                                  // empty
		`not json`,                                          // malformed
		`{"expectedRevision":0,"contentType":null,"labels":{}} {}`, // trailing content
		`{"expectedRevision":0,"contentType":null}`,         // missing labels
		`{"expectedRevision":0,"labels":{}}`,                // missing contentType
		`{"contentType":null,"labels":{}}`,                  // missing expectedRevision
		`{"expectedRevision":0,"contentType":null,"labels":{},"x":1}`, // unknown field
		`{"expectedRevision":0,"expectedRevision":0,"contentType":null,"labels":{}}`, // duplicate field
		`{"expectedRevision":"0","contentType":null,"labels":{}}`,   // string revision
		`{"expectedRevision":-1,"contentType":null,"labels":{}}`,    // negative
		`{"expectedRevision":1.5,"contentType":null,"labels":{}}`,   // fractional
		`{"expectedRevision":null,"contentType":null,"labels":{}}`,  // null revision
		`{"expectedRevision":0,"contentType":1,"labels":{}}`,        // non-string contentType
		`{"expectedRevision":0,"contentType":null,"labels":null}`,   // null labels
		`{"expectedRevision":0,"contentType":null,"labels":[]}`,     // labels not object
		`{"expectedRevision":0,"contentType":null,"labels":{"a":1}}`, // non-string value
	}
	for _, body := range badRequest {
		rec := putMetadata(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
			t.Fatalf("body %s: status = %d code = %s, want 400 invalid_request", body, rec.Code, errorCode(t, rec))
		}
	}

	longContentType := strings.Repeat("x", 256)
	longValue := strings.Repeat("v", 257)
	manyLabels := make(map[string]string)
	for i := 0; i < 65; i++ {
		manyLabels[fmt.Sprintf("k%02d", i)] = "v"
	}
	manyLabelsJSON, _ := json.Marshal(manyLabels)
	unprocessable := []string{
		`{"expectedRevision":0,"contentType":"","labels":{}}`,                    // empty contentType
		`{"expectedRevision":0,"contentType":"` + longContentType + `","labels":{}}`, // too long
		`{"expectedRevision":0,"contentType":"a\u0009b","labels":{}}`,            // control char
		`{"expectedRevision":0,"contentType":null,"labels":{"A":"v"}}`,           // uppercase key
		`{"expectedRevision":0,"contentType":null,"labels":{"-a":"v"}}`,          // bad first char
		`{"expectedRevision":0,"contentType":null,"labels":{"` + strings.Repeat("k", 65) + `":"v"}}`, // key too long
		`{"expectedRevision":0,"contentType":null,"labels":{"a":"` + longValue + `"}}`,             // value too long
		`{"expectedRevision":0,"contentType":null,"labels":{"a":"x\u007f"}}`,     // control char value
		`{"expectedRevision":0,"contentType":null,"labels":` + string(manyLabelsJSON) + `}`,        // too many labels
	}
	for _, body := range unprocessable {
		rec := putMetadata(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "invalid_metadata" {
			t.Fatalf("body %s: status = %d code = %s, want 422 invalid_metadata", body, rec.Code, errorCode(t, rec))
		}
	}

	// Boundary values are accepted: 255-code-point contentType, 64-char key,
	// 256-code-point value, 64 labels, unicode content.
	labels := make(map[string]string)
	for i := 0; i < 64; i++ {
		labels[fmt.Sprintf("k%02d", i)] = strings.Repeat("界", 256)
	}
	labels[strings.Repeat("z", 64)] = "v"
	delete(labels, "k00")
	labelsJSON, _ := json.Marshal(labels)
	body := `{"expectedRevision":0,"contentType":"` + strings.Repeat("é", 255) + `","labels":` + string(labelsJSON) + `}`
	rec = putMetadata(t, h, obj.CID, body, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("boundary write status = %d code = %s, want 201", rec.Code, errorCode(t, rec))
	}
}

func TestMetadataGetValidation(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-get"))

	rec := getMetadata(t, h, "bad-cid", "")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("bad cid: status = %d code = %s", rec.Code, errorCode(t, rec))
	}
	rec = getMetadata(t, h, obj.CID, "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "metadata_not_found" {
		t.Fatalf("no metadata: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")

	badQueries := []string{"?revision=0", "?revision=-1", "?revision=1.5", "?revision=x", "?revision=", "?revision=1&revision=1", "?other=1", "?revision=2&other=1"}
	for _, q := range badQueries {
		rec := getMetadata(t, h, obj.CID, q)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
			t.Fatalf("query %s: status = %d code = %s, want 400 invalid_request", q, rec.Code, errorCode(t, rec))
		}
	}
	rec = getMetadata(t, h, obj.CID, "?revision=2")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "metadata_revision_not_found" {
		t.Fatalf("missing revision: status = %d code = %s", rec.Code, errorCode(t, rec))
	}
	rec = getMetadata(t, h, obj.CID, "?revision=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("revision 1 status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMetadataGarbageCollection(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-gc"))
	putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{}}`, "application/json")

	// A dry run keeps the history.
	if code, _ := runGC(t, h, "?dryRun=true"); code != http.StatusOK {
		t.Fatalf("dry run status = %d", code)
	}
	if rec := getMetadata(t, h, obj.CID, ""); rec.Code != http.StatusOK {
		t.Fatalf("metadata after dry run status = %d, want 200", rec.Code)
	}

	// A real sweep deletes the history atomically with the object.
	if code, _ := runGC(t, h, ""); code != http.StatusOK {
		t.Fatalf("gc status = %d", code)
	}
	if rec := getMetadata(t, h, obj.CID, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "metadata_not_found" {
		t.Fatalf("metadata after gc: status = %d code = %s", rec.Code, errorCode(t, rec))
	}

	// Re-uploading the same content does not restore the old history.
	again := upload(t, h, []byte("meta-gc"))
	if again.CID != obj.CID {
		t.Fatalf("re-upload cid = %s, want %s", again.CID, obj.CID)
	}
	if rec := getMetadata(t, h, obj.CID, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "metadata_not_found" {
		t.Fatalf("metadata after re-upload: status = %d code = %s", rec.Code, errorCode(t, rec))
	}
	// The next write starts a fresh history at revision 1.
	rec := putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":null,"labels":{}}`, "application/json")
	if rec.Code != http.StatusCreated || decodeMetadataResponse(t, rec).Revision != 1 {
		t.Fatalf("write after re-upload: status = %d, want 201 revision 1", rec.Code)
	}
}

func TestMetadataSideEffects(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-side"))
	before := listPins(t, h)
	statsBefore := getStats(t, h)

	putMetadata(t, h, obj.CID, `{"expectedRevision":0,"contentType":"text/plain","labels":{"a":"1"}}`, "application/json")

	// No pin is established and storage statistics are unchanged.
	if after := listPins(t, h); len(after.Pins) != len(before.Pins) {
		t.Fatalf("pins changed: %v -> %v", before.Pins, after.Pins)
	}
	if statsAfter := getStats(t, h); statsAfter != statsBefore {
		t.Fatalf("stats changed: %+v -> %+v", statsBefore, statsAfter)
	}
	// Metadata writes produce no audit events.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))
	var audit auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("audit response is not JSON: %v", err)
	}
	for _, ev := range audit.Events {
		if ev.Action != "object.upload" {
			t.Fatalf("unexpected audit event %q", ev.Action)
		}
	}
	// Metadata does not protect the object from collection.
	if code, _ := runGC(t, h, ""); code != http.StatusOK {
		t.Fatalf("gc status = %d", code)
	}
	if rec := getMetadata(t, h, obj.CID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("metadata after gc status = %d, want 404", rec.Code)
	}
}

func TestMetadataMethodNotAllowed(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("meta-methods"))
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/objects/"+obj.CID+"/metadata", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%s: status = %d code = %s", method, rec.Code, errorCode(t, rec))
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, PUT" {
			t.Fatalf("%s: Allow = %q, want %q", method, allow, "GET, PUT")
		}
	}
}
