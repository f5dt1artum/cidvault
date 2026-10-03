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

// storeObject uploads a body and accepts both the 201 first-store and the
// 200 duplicate response.
func storeObject(t *testing.T, h http.Handler, body []byte) objectResponse {
	t.Helper()
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
	}
	return decodeObjectResponse(t, rec)
}

func getAuditEvents(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, auditResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp auditResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("audit response is not JSON: %v", err)
		}
	}
	return rec, resp
}

func checkAuditError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body["error"]["code"] != code {
		t.Fatalf("error code = %q, want %q", body["error"]["code"], code)
	}
}

func TestAuditEmptyLog(t *testing.T) {
	h := Handler()
	rec, resp := getAuditEvents(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if resp.Events == nil || len(resp.Events) != 0 {
		t.Fatalf("events = %v, want empty array", resp.Events)
	}
	if resp.NextAfter != 0 || resp.HasMore {
		t.Fatalf("nextAfter = %d, hasMore = %v, want 0 and false", resp.NextAfter, resp.HasMore)
	}
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("events should serialize as an empty array: %s", rec.Body.String())
	}
}

func TestAuditRecordsObjectUpload(t *testing.T) {
	h := Handler()
	body := []byte("audit me")
	resp := uploadObject(t, h, body)
	storeObject(t, h, body) // duplicate: existing

	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	first, second := page.Events[0], page.Events[1]
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("seqs = %d, %d, want 1, 2", first.Seq, second.Seq)
	}
	if first.Action != "object.upload" || first.Result != "created" {
		t.Fatalf("first = %s/%s, want object.upload/created", first.Action, first.Result)
	}
	if second.Action != "object.upload" || second.Result != "existing" {
		t.Fatalf("second = %s/%s, want object.upload/existing", second.Action, second.Result)
	}
	for i, ev := range []auditEvent{first, second} {
		if ev.CID == nil || *ev.CID != resp.CID {
			t.Fatalf("event %d cid = %v, want %s", i, ev.CID, resp.CID)
		}
		if ev.Bytes == nil || *ev.Bytes != len(body) {
			t.Fatalf("event %d bytes = %v, want %d", i, ev.Bytes, len(body))
		}
		if ev.Objects != nil || ev.ProviderID != nil {
			t.Fatalf("event %d objects/providerId should be null: %+v", i, ev)
		}
		if ev.At.Location() != time.UTC {
			t.Fatalf("event %d at location = %v, want UTC", i, ev.At.Location())
		}
	}
	if page.NextAfter != 2 || page.HasMore {
		t.Fatalf("nextAfter = %d, hasMore = %v, want 2 and false", page.NextAfter, page.HasMore)
	}

	// Inapplicable fields serialize as explicit nulls.
	var raw struct {
		Events []map[string]json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(mustAuditBody(t, h, "?limit=1"), &raw); err != nil {
		t.Fatalf("raw events: %v", err)
	}
	if len(raw.Events) != 1 {
		t.Fatalf("raw events = %d, want 1", len(raw.Events))
	}
	for _, key := range []string{"seq", "at", "action", "cid", "result", "objects", "bytes", "providerId"} {
		if _, ok := raw.Events[0][key]; !ok {
			t.Fatalf("event is missing key %q: %v", key, raw.Events[0])
		}
	}
	if string(raw.Events[0]["objects"]) != "null" || string(raw.Events[0]["providerId"]) != "null" {
		t.Fatalf("objects/providerId should be null: %v", raw.Events[0])
	}
}

func mustAuditBody(t *testing.T, h http.Handler, query string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestAuditSkipsFailuresAndReads(t *testing.T) {
	h := Handler()
	// Failed requests: wrong media type, bad CID, missing object, no provider.
	rec := postBody(t, h, []byte("x"), "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("upload status = %d", rec.Code)
	}
	for _, path := range []string{
		"/v1/objects/not-a-cid",
		"/v1/objects/" + strings.Repeat("0", 63) + "g",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	missing := newObject([]byte("missing")).cid
	if rec := retrievalRequest(t, h, http.MethodPost, missing); rec.Code != http.StatusNotFound {
		t.Fatalf("retrieval status = %d", rec.Code)
	}
	if rec := putPin(t, h, missing, "{}", "application/json"); rec.Code != http.StatusNotFound {
		t.Fatalf("pin status = %d", rec.Code)
	}
	// Audit reads are not recorded either.
	getAuditEvents(t, h, "")
	getAuditEvents(t, h, "?limit=1")

	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 0 {
		t.Fatalf("events = %v, want none", page.Events)
	}
}

func TestAuditPinsAndGC(t *testing.T) {
	h := Handler()
	resp := uploadObject(t, h, []byte("pinned then collected"))

	if rec := putPin(t, h, resp.CID, "{}", "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d", rec.Code)
	}
	if rec := putPin(t, h, resp.CID, "{}", "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("repin status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/pins/"+resp.CID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unpin status = %d", rec.Code)
	}
	gc := func(query string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/gc"+query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("gc %s status = %d", query, rec.Code)
		}
	}
	gc("?dryRun=true")
	gc("")

	_, page := getAuditEvents(t, h, "?limit=10")
	if len(page.Events) != 6 {
		t.Fatalf("events = %d, want 6: %+v", len(page.Events), page.Events)
	}
	type want struct {
		action, result string
	}
	wants := []want{
		{"object.upload", "created"},
		{"pin.put", "created"},
		{"pin.put", "updated"},
		{"pin.delete", "deleted"},
		{"gc.preview", "previewed"},
		{"gc.collect", "collected"},
	}
	for i, w := range wants {
		ev := page.Events[i]
		if ev.Action != w.action || ev.Result != w.result {
			t.Fatalf("event %d = %s/%s, want %s/%s", i, ev.Action, ev.Result, w.action, w.result)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
	for _, i := range []int{4, 5} {
		ev := page.Events[i]
		if ev.CID != nil || ev.ProviderID != nil {
			t.Fatalf("gc event %d cid/providerId should be null: %+v", i, ev)
		}
		if ev.Objects == nil || *ev.Objects != 1 || ev.Bytes == nil || *ev.Bytes != len("pinned then collected") {
			t.Fatalf("gc event %d objects/bytes = %v/%v, want 1/%d", i, ev.Objects, ev.Bytes, len("pinned then collected"))
		}
	}
}

func TestAuditProviders(t *testing.T) {
	h := Handler()
	cid := newObject([]byte("provider target")).cid
	expiry := time.Now().Add(time.Hour)
	if rec := publishProvider(t, h, cid, "node-a", providerBody([]string{"https://a.example/"}, expiry)); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := publishProvider(t, h, cid, "node-a", providerBody([]string{"https://b.example/"}, expiry)); rec.Code != http.StatusOK {
		t.Fatalf("republish status = %d: %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/node-a", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}

	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	results := []string{"created", "updated", "deleted"}
	actions := []string{auditProviderPut, auditProviderPut, auditProviderDelete}
	for i, ev := range page.Events {
		if ev.Action != actions[i] || ev.Result != results[i] {
			t.Fatalf("event %d = %s/%s, want %s/%s", i, ev.Action, ev.Result, actions[i], results[i])
		}
		if ev.CID == nil || *ev.CID != cid {
			t.Fatalf("event %d cid = %v, want %s", i, ev.CID, cid)
		}
		if ev.ProviderID == nil || *ev.ProviderID != "node-a" {
			t.Fatalf("event %d providerId = %v, want node-a", i, ev.ProviderID)
		}
		if ev.Objects != nil || ev.Bytes != nil {
			t.Fatalf("event %d objects/bytes should be null: %+v", i, ev)
		}
	}
}

func TestAuditRetrieval(t *testing.T) {
	h := Handler()
	body := []byte(strings.Repeat("fetch-me", 1000))
	want := newObject(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishProvider(t, h, want.cid, "node-a",
		providerBody([]string{srv.URL}, time.Now().Add(time.Hour)))

	if rec := retrievalRequest(t, h, http.MethodPost, want.cid); rec.Code != http.StatusCreated {
		t.Fatalf("retrieval status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := retrievalRequest(t, h, http.MethodPost, want.cid); rec.Code != http.StatusOK {
		t.Fatalf("second retrieval status = %d", rec.Code)
	}

	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(page.Events), page.Events)
	}
	fetch, local := page.Events[1], page.Events[2]
	if fetch.Action != "retrieval.fetch" || fetch.Result != "retrieved_created" {
		t.Fatalf("fetch = %s/%s, want retrieval.fetch/retrieved_created", fetch.Action, fetch.Result)
	}
	if fetch.ProviderID == nil || *fetch.ProviderID != "node-a" {
		t.Fatalf("fetch providerId = %v, want node-a", fetch.ProviderID)
	}
	if fetch.Bytes == nil || *fetch.Bytes != len(body) {
		t.Fatalf("fetch bytes = %v, want %d", fetch.Bytes, len(body))
	}
	if local.Action != "retrieval.local" || local.Result != "local" {
		t.Fatalf("local = %s/%s, want retrieval.local/local", local.Action, local.Result)
	}
	if local.ProviderID != nil {
		t.Fatalf("local providerId = %v, want null", local.ProviderID)
	}
	if local.Bytes == nil || *local.Bytes != len(body) {
		t.Fatalf("local bytes = %v, want %d", local.Bytes, len(body))
	}
}

func TestAuditPagination(t *testing.T) {
	h := Handler()
	for i := 0; i < 5; i++ {
		uploadObject(t, h, []byte(fmt.Sprintf("object-%d", i)))
	}

	_, page := getAuditEvents(t, h, "?limit=2")
	if len(page.Events) != 2 || page.Events[0].Seq != 1 || page.Events[1].Seq != 2 {
		t.Fatalf("page 1 = %+v, want seqs 1,2", page.Events)
	}
	if page.NextAfter != 2 || !page.HasMore {
		t.Fatalf("page 1 nextAfter = %d, hasMore = %v, want 2 and true", page.NextAfter, page.HasMore)
	}

	_, page = getAuditEvents(t, h, "?after=2&limit=2")
	if len(page.Events) != 2 || page.Events[0].Seq != 3 || page.Events[1].Seq != 4 {
		t.Fatalf("page 2 = %+v, want seqs 3,4", page.Events)
	}
	if page.NextAfter != 4 || !page.HasMore {
		t.Fatalf("page 2 nextAfter = %d, hasMore = %v, want 4 and true", page.NextAfter, page.HasMore)
	}

	_, page = getAuditEvents(t, h, "?after=4")
	if len(page.Events) != 1 || page.Events[0].Seq != 5 {
		t.Fatalf("page 3 = %+v, want seq 5", page.Events)
	}
	if page.NextAfter != 5 || page.HasMore {
		t.Fatalf("page 3 nextAfter = %d, hasMore = %v, want 5 and false", page.NextAfter, page.HasMore)
	}

	// An empty page keeps the incoming cursor.
	_, page = getAuditEvents(t, h, "?after=5")
	if len(page.Events) != 0 || page.NextAfter != 5 || page.HasMore {
		t.Fatalf("empty page = %+v, want nextAfter 5 and hasMore false", page)
	}
	// A cursor beyond the end is not expired.
	_, page = getAuditEvents(t, h, "?after=999")
	if len(page.Events) != 0 || page.NextAfter != 999 || page.HasMore {
		t.Fatalf("tail page = %+v, want nextAfter 999 and hasMore false", page)
	}
	// Ascending order across a full read.
	_, page = getAuditEvents(t, h, "?limit=1000")
	for i, ev := range page.Events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestAuditBadRequests(t *testing.T) {
	h := Handler()
	uploadObject(t, h, []byte("x"))
	queries := []string{
		"?after=1&after=2",
		"?limit=1&limit=2",
		"?after=1&limit=1&after=3",
		"?foo=1",
		"?after=-1",
		"?after=abc",
		"?after=1.5",
		"?after=",
		"?after=+1",
		"?after=99999999999999999999999",
		"?limit=0",
		"?limit=1001",
		"?limit=-5",
		"?limit=1.0",
		"?limit=",
	}
	for _, q := range queries {
		rec, _ := getAuditEvents(t, h, q)
		checkAuditError(t, rec, http.StatusBadRequest, "invalid_request")
	}
	// None of the failed reads were recorded.
	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
}

func TestAuditMethodNotAllowed(t *testing.T) {
	h := Handler()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/audit/events", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		checkAuditError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s Allow = %q, want GET", method, allow)
		}
	}
}

func TestAuditLogEvictionKeepsRecent(t *testing.T) {
	a := newAuditLog()
	for i := 0; i < auditCapacity+5; i++ {
		a.append(auditRecord{action: auditPinDelete, result: "deleted"})
	}
	events, hasMore, expired := a.snapshot(0, auditCapacity+10)
	if expired || hasMore {
		t.Fatalf("expired = %v, hasMore = %v, want both false", expired, hasMore)
	}
	if len(events) != auditCapacity {
		t.Fatalf("retained = %d, want %d", len(events), auditCapacity)
	}
	if events[0].Seq != 6 || events[len(events)-1].Seq != int64(auditCapacity+5) {
		t.Fatalf("retained seqs %d..%d, want 6..%d",
			events[0].Seq, events[len(events)-1].Seq, auditCapacity+5)
	}
	// A cursor before the oldest retained predecessor is expired; the
	// predecessor itself is still valid.
	if _, _, expired := a.snapshot(4, 1); !expired {
		t.Fatal("after=4 should be expired")
	}
	if _, _, expired := a.snapshot(5, 1); expired {
		t.Fatal("after=5 should still be valid")
	}
	// An empty log never expires a cursor.
	fresh := newAuditLog()
	if _, _, expired := fresh.snapshot(99, 1); expired {
		t.Fatal("empty log should not expire cursors")
	}
	// Sequence numbers keep increasing after eviction.
	a.append(auditRecord{action: auditPinDelete, result: "deleted"})
	events, _, _ = a.snapshot(int64(auditCapacity+4), 10)
	if len(events) != 2 || events[0].Seq != int64(auditCapacity+5) || events[1].Seq != int64(auditCapacity+6) {
		t.Fatalf("events after eviction = %+v", events)
	}
}

func TestAuditCursorExpiredOverHTTP(t *testing.T) {
	h := Handler()
	body := []byte("x")
	for i := 0; i < auditCapacity+2; i++ {
		storeObject(t, h, body)
	}
	// The oldest retained seq is 3; after=1 points before its predecessor.
	rec, _ := getAuditEvents(t, h, "?after=1")
	checkAuditError(t, rec, http.StatusGone, "audit_cursor_expired")
	// after=2 is the predecessor of the oldest retained event: still valid.
	rec, page := getAuditEvents(t, h, "?after=2&limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 3 {
		t.Fatalf("first retained event = %+v, want seq 3", page.Events)
	}
	if !page.HasMore {
		t.Fatal("hasMore = false, want true")
	}
	// Default limit applies.
	_, page = getAuditEvents(t, h, "")
	if len(page.Events) != 100 {
		t.Fatalf("default page = %d events, want 100", len(page.Events))
	}
}

func TestAuditReadsDoNotRecord(t *testing.T) {
	h := Handler()
	uploadObject(t, h, bytes.Repeat([]byte("y"), 10))
	for i := 0; i < 3; i++ {
		getAuditEvents(t, h, "")
	}
	_, page := getAuditEvents(t, h, "")
	if len(page.Events) != 1 || page.Events[0].Seq != 1 {
		t.Fatalf("events = %+v, want exactly the upload", page.Events)
	}
}

func TestAuditConcurrentSameObject(t *testing.T) {
	h := Handler()
	body := []byte("concurrent upload")
	const n = 16
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postBody(t, h, body, "application/octet-stream")
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)
	created201 := 0
	for code := range codes {
		if code == http.StatusCreated {
			created201++
		} else if code != http.StatusOK {
			t.Fatalf("upload status = %d", code)
		}
	}
	if created201 != 1 {
		t.Fatalf("201 responses = %d, want 1", created201)
	}

	_, page := getAuditEvents(t, h, "?limit=1000")
	if len(page.Events) != n {
		t.Fatalf("events = %d, want %d", len(page.Events), n)
	}
	createdEvents := 0
	for i, ev := range page.Events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
		if ev.Result == "created" {
			createdEvents++
			if i != 0 {
				t.Fatalf("created event at index %d, want it to precede every existing event", i)
			}
		} else if ev.Result != "existing" {
			t.Fatalf("event %d result = %q", i, ev.Result)
		}
	}
	if createdEvents != 1 {
		t.Fatalf("created events = %d, want 1", createdEvents)
	}
}
