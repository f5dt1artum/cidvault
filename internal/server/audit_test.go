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

// auditEventJSON decodes one event with pointer fields so null checks are
// observable in tests.
type auditEventJSON struct {
	Seq        int64   `json:"seq"`
	At         string  `json:"at"`
	Action     string  `json:"action"`
	CID        *string `json:"cid"`
	Result     string  `json:"result"`
	Objects    *int    `json:"objects"`
	Bytes      *int    `json:"bytes"`
	ProviderID *string `json:"providerId"`
}

type auditEventsPage struct {
	Events    []auditEventJSON `json:"events"`
	NextAfter int64            `json:"nextAfter"`
	HasMore   bool             `json:"hasMore"`
}

func getAuditPage(t *testing.T, h http.Handler, query string) (int, auditEventsPage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var page auditEventsPage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("response is not JSON: %v", err)
		}
	}
	return rec.Code, page
}

func TestAuditEmptyLog(t *testing.T) {
	h := Handler()
	code, page := getAuditPage(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if page.Events == nil || len(page.Events) != 0 {
		t.Fatalf("events = %v, want empty array", page.Events)
	}
	if strings.Contains(strings.ReplaceAll(recBody(t, h, ""), " ", ""), `"events":null`) {
		t.Fatal("events must be an empty array, not null")
	}
	if page.NextAfter != 0 || page.HasMore {
		t.Fatalf("nextAfter = %d, hasMore = %v; want 0, false", page.NextAfter, page.HasMore)
	}
}

func recBody(t *testing.T, h http.Handler, query string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestAuditObjectUploadEvents(t *testing.T) {
	h := Handler()
	body := []byte("audit me")
	first := uploadObject(t, h, body)
	second := postBody(t, h, body, "application/octet-stream")
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate upload status = %d, want 200", second.Code)
	}

	code, page := getAuditPage(t, h, "")
	if code != http.StatusOK || len(page.Events) != 2 {
		t.Fatalf("code = %d, events = %d; want 200, 2", code, len(page.Events))
	}
	for i, e := range page.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Action != "object.upload" {
			t.Fatalf("event %d action = %q", i, e.Action)
		}
		if e.CID == nil || *e.CID != first.CID {
			t.Fatalf("event %d cid = %v", i, e.CID)
		}
		if e.Objects == nil || *e.Objects != 1 {
			t.Fatalf("event %d objects = %v", i, e.Objects)
		}
		if e.Bytes == nil || *e.Bytes != len(body) {
			t.Fatalf("event %d bytes = %v", i, e.Bytes)
		}
		if e.ProviderID != nil {
			t.Fatalf("event %d providerId = %v, want null", i, *e.ProviderID)
		}
		at, err := time.Parse(time.RFC3339Nano, e.At)
		if err != nil {
			t.Fatalf("event %d at %q does not parse as RFC3339Nano: %v", i, e.At, err)
		}
		if at.Location() != time.UTC {
			t.Fatalf("event %d at %q is not UTC", i, e.At)
		}
	}
	if page.Events[0].Result != "created" || page.Events[1].Result != "existing" {
		t.Fatalf("results = %q, %q; want created, existing",
			page.Events[0].Result, page.Events[1].Result)
	}
	if page.NextAfter != 2 || page.HasMore {
		t.Fatalf("nextAfter = %d, hasMore = %v; want 2, false", page.NextAfter, page.HasMore)
	}
}

func TestAuditPinAndGCEvents(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("pin and sweep"))

	putPin := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/pins/"+obj.CID, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := putPin(); rec.Code != http.StatusCreated {
		t.Fatalf("pin put status = %d", rec.Code)
	}
	if rec := putPin(); rec.Code != http.StatusOK {
		t.Fatalf("pin re-put status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pin delete status = %d", rec.Code)
	}
	// A failing delete must not be recorded.
	req = httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pin re-delete status = %d", rec.Code)
	}

	for _, query := range []string{"?dryRun=true", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/gc"+query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("gc %s status = %d", query, rec.Code)
		}
	}

	_, page := getAuditPage(t, h, "")
	want := []struct {
		action, result string
	}{
		{"object.upload", "created"},
		{"pin.put", "created"},
		{"pin.put", "updated"},
		{"pin.delete", "deleted"},
		{"gc.preview", "previewed"},
		{"gc.collect", "collected"},
	}
	if len(page.Events) != len(want) {
		t.Fatalf("events = %d, want %d", len(page.Events), len(want))
	}
	for i, w := range want {
		e := page.Events[i]
		if e.Action != w.action || e.Result != w.result {
			t.Fatalf("event %d = %s/%s, want %s/%s", i, e.Action, e.Result, w.action, w.result)
		}
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d", i, e.Seq)
		}
	}
	// GC events carry the response totals and no cid.
	gc := page.Events[5]
	if gc.CID != nil {
		t.Fatalf("gc cid = %v, want null", *gc.CID)
	}
	if gc.Objects == nil || *gc.Objects != 1 || gc.Bytes == nil || *gc.Bytes != len("pin and sweep") {
		t.Fatalf("gc objects/bytes = %v/%v, want 1/%d", gc.Objects, gc.Bytes, len("pin and sweep"))
	}
}

func TestAuditProviderEvents(t *testing.T) {
	h := Handler()
	cid := expectedRootCID(0, nil)
	body := fmt.Sprintf(`{"addresses":["https://a.example/"],"expiresAt":%q}`,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	put := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/v1/providers/"+cid+"/p1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := put(); rec.Code != http.StatusCreated {
		t.Fatalf("provider put status = %d", rec.Code)
	}
	if rec := put(); rec.Code != http.StatusOK {
		t.Fatalf("provider re-put status = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/p1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("provider delete status = %d", rec.Code)
	}

	_, page := getAuditPage(t, h, "")
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	wantResults := []string{"created", "updated", "deleted"}
	wantActions := []string{"provider.put", "provider.put", "provider.delete"}
	for i, e := range page.Events {
		if e.Action != wantActions[i] || e.Result != wantResults[i] {
			t.Fatalf("event %d = %s/%s", i, e.Action, e.Result)
		}
		if e.CID == nil || *e.CID != cid {
			t.Fatalf("event %d cid = %v", i, e.CID)
		}
		if e.ProviderID == nil || *e.ProviderID != "p1" {
			t.Fatalf("event %d providerId = %v", i, e.ProviderID)
		}
		if e.Objects != nil || e.Bytes != nil {
			t.Fatalf("event %d objects/bytes should be null, got %v/%v", i, e.Objects, e.Bytes)
		}
	}
}

func TestAuditRetrievalEvents(t *testing.T) {
	h := Handler()
	payload := []byte("fetch me from a provider")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer origin.Close()

	cid := expectedRootCID(len(payload), []string{chunkCID(payload)})
	providerBody := fmt.Sprintf(`{"addresses":[%q],"expiresAt":%q}`,
		origin.URL, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/"+cid+"/p1", strings.NewReader(providerBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("provider put status = %d", rec.Code)
	}

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/retrievals/"+cid, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(); rec.Code != http.StatusCreated {
		t.Fatalf("retrieval status = %d", rec.Code)
	}
	if rec := post(); rec.Code != http.StatusOK {
		t.Fatalf("local retrieval status = %d", rec.Code)
	}

	_, page := getAuditPage(t, h, "")
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	fetch := page.Events[1]
	if fetch.Action != "retrieval.fetch" || fetch.Result != "retrieved_created" {
		t.Fatalf("fetch event = %s/%s", fetch.Action, fetch.Result)
	}
	if fetch.ProviderID == nil || *fetch.ProviderID != "p1" {
		t.Fatalf("fetch providerId = %v", fetch.ProviderID)
	}
	if fetch.Bytes == nil || *fetch.Bytes != len(payload) {
		t.Fatalf("fetch bytes = %v", fetch.Bytes)
	}
	local := page.Events[2]
	if local.Action != "retrieval.local" || local.Result != "local" {
		t.Fatalf("local event = %s/%s", local.Action, local.Result)
	}
	if local.ProviderID != nil {
		t.Fatalf("local providerId = %v, want null", *local.ProviderID)
	}
}

func TestAuditFailuresNotRecorded(t *testing.T) {
	h := Handler()
	uploadObject(t, h, []byte("kept"))

	// A failing upload (bad media type), a bad-cid pin and a failed gc
	// parameter must not add events.
	postBody(t, h, []byte("x"), "text/plain")
	req := httptest.NewRequest(http.MethodPut, "/v1/pins/not-a-cid", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	req = httptest.NewRequest(http.MethodPost, "/v1/gc?dryRun=yes", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	_, page := getAuditPage(t, h, "")
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1 (failures must not be recorded)", len(page.Events))
	}
}

func TestAuditPagination(t *testing.T) {
	h := Handler()
	for i := 0; i < 5; i++ {
		uploadObject(t, h, []byte(fmt.Sprintf("object-%d", i)))
	}

	code, page := getAuditPage(t, h, "?limit=2")
	if code != http.StatusOK || len(page.Events) != 2 {
		t.Fatalf("code = %d, events = %d", code, len(page.Events))
	}
	if page.Events[0].Seq != 1 || page.NextAfter != 2 || !page.HasMore {
		t.Fatalf("page 1: seqs %d.., nextAfter = %d, hasMore = %v",
			page.Events[0].Seq, page.NextAfter, page.HasMore)
	}

	code, page = getAuditPage(t, h, "?after=2&limit=2")
	if code != http.StatusOK || len(page.Events) != 2 {
		t.Fatalf("code = %d, events = %d", code, len(page.Events))
	}
	if page.Events[0].Seq != 3 || page.Events[1].Seq != 4 || page.NextAfter != 4 || !page.HasMore {
		t.Fatalf("page 2: nextAfter = %d, hasMore = %v", page.NextAfter, page.HasMore)
	}

	code, page = getAuditPage(t, h, "?after=4&limit=1000")
	if code != http.StatusOK || len(page.Events) != 1 || page.HasMore {
		t.Fatalf("page 3: events = %d, hasMore = %v", len(page.Events), page.HasMore)
	}

	// An empty page beyond the newest event keeps the cursor.
	code, page = getAuditPage(t, h, "?after=5")
	if code != http.StatusOK || len(page.Events) != 0 {
		t.Fatalf("code = %d, events = %d", code, len(page.Events))
	}
	if page.NextAfter != 5 || page.HasMore {
		t.Fatalf("empty page: nextAfter = %d, hasMore = %v", page.NextAfter, page.HasMore)
	}
}

func TestAuditQueryValidation(t *testing.T) {
	h := Handler()
	uploadObject(t, h, []byte("x"))

	bad := []string{
		"?after=1&after=2", // duplicate parameter
		"?limit=1&limit=2", // duplicate parameter
		"?bogus=1",         // unknown parameter
		"?after=abc",       // non-integer cursor
		"?after=",          // empty cursor
		"?after=-1",        // negative cursor
		"?limit=0",         // below range
		"?limit=1001",      // above range
		"?limit=1.5",       // non-integer limit
		"?limit=",          // empty limit
	}
	for _, query := range bad {
		req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+query, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status = %d, want 400", query, rec.Code)
		}
		var body map[string]map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil ||
			body["error"]["code"] != "invalid_request" {
			t.Fatalf("GET %s body = %s, want invalid_request", query, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audit/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", rec.Header().Get("Allow"))
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil ||
		body["error"]["code"] != "method_not_allowed" {
		t.Fatalf("POST body = %s, want method_not_allowed", rec.Body.String())
	}
}

func TestAuditCursorExpiryAndEviction(t *testing.T) {
	l := newAuditLog()
	cid := "sha256:" + strings.Repeat("0", 64)
	total := auditCapacity + 10
	for i := 0; i < total; i++ {
		l.append(objectAuditEvent(auditObjectUpload, "created", cid, 1, nil))
	}
	if len(l.events) != auditCapacity {
		t.Fatalf("retained = %d, want %d", len(l.events), auditCapacity)
	}
	// Sequence numbers continue across eviction.
	if l.events[0].Seq != 11 || l.events[len(l.events)-1].Seq != int64(total) {
		t.Fatalf("retained seqs %d..%d, want 11..%d",
			l.events[0].Seq, l.events[len(l.events)-1].Seq, total)
	}

	// A cursor at the seq before the oldest retained event is still valid.
	_, _, _, expired := l.page(10, 1)
	if expired {
		t.Fatal("after=10 must not expire (oldest retained is 11)")
	}
	// Anything older is gone.
	if _, _, _, expired = l.page(9, 1); !expired {
		t.Fatal("after=9 must expire")
	}

	// The same semantics are exposed over HTTP.
	h := http.NewServeMux()
	h.HandleFunc("/v1/audit/events", l.getEvents)
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events?after=9", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", rec.Code)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil ||
		body["error"]["code"] != "audit_cursor_expired" {
		t.Fatalf("body = %s, want audit_cursor_expired", rec.Body.String())
	}

	code, page := getAuditPage(t, h, "?after=10&limit=3")
	if code != http.StatusOK || len(page.Events) != 3 || page.Events[0].Seq != 11 {
		t.Fatalf("code = %d, events = %v", code, page.Events)
	}
}

func TestAuditEventVisibleAfterCommit(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("committed"))

	// The state the event describes is observable once the event is.
	_, page := getAuditPage(t, h, "")
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte("committed")) {
		t.Fatalf("object readback failed: %d", rec.Code)
	}
}

func TestAuditBundleAndDeltaImportEvents(t *testing.T) {
	h := Handler()
	payload := []byte("bundle import audit")
	doc, _ := buildBundle(t, payload)

	// Full bundle import: created, then existing on repeat.
	body := marshalBundle(t, doc)
	for i, want := range []int{http.StatusCreated, http.StatusOK} {
		req := httptest.NewRequest(http.MethodPost, "/v1/bundles", bytes.NewReader(body))
		req.Header.Set("Content-Type", bundleMediaType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("bundle import %d status = %d, want %d", i, rec.Code, want)
		}
	}

	// Delta bundle import of an empty object needs no blocks.
	emptyDoc := bundleDocument{
		Version: bundleVersion,
		Root: bundleRoot{
			CID:       expectedRootCID(0, nil),
			Size:      0,
			ChunkSize: ChunkSize,
			Chunks:    []string{},
		},
		Blocks: []bundleBlock{},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/delta-bundles",
		bytes.NewReader(marshalBundle(t, emptyDoc)))
	req.Header.Set("Content-Type", deltaBundleMediaType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("delta import status = %d, want 201", rec.Code)
	}

	_, page := getAuditPage(t, h, "")
	want := []struct {
		action, result string
	}{
		{"bundle.import", "created"},
		{"bundle.import", "existing"},
		{"delta_bundle.import", "created"},
	}
	if len(page.Events) != len(want) {
		t.Fatalf("events = %d, want %d", len(page.Events), len(want))
	}
	for i, w := range want {
		e := page.Events[i]
		if e.Action != w.action || e.Result != w.result {
			t.Fatalf("event %d = %s/%s, want %s/%s", i, e.Action, e.Result, w.action, w.result)
		}
	}
	if e := page.Events[0]; e.Bytes == nil || *e.Bytes != len(payload) {
		t.Fatalf("bundle import bytes = %v, want %d", e.Bytes, len(payload))
	}
}

func TestAuditConcurrentCommitOrder(t *testing.T) {
	h := Handler()
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uploadObject(t, h, []byte(fmt.Sprintf("concurrent-%d", i)))
		}(i)
	}
	wg.Wait()

	_, page := getAuditPage(t, h, "?limit=1000")
	if len(page.Events) != n {
		t.Fatalf("events = %d, want %d", len(page.Events), n)
	}
	for i, e := range page.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want strictly increasing from 1", i, e.Seq)
		}
		if e.Action != "object.upload" || e.Result != "created" {
			t.Fatalf("event %d = %s/%s", i, e.Action, e.Result)
		}
	}
}
