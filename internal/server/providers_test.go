package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testCID = "sha256:" +
	"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func providerRequest(t *testing.T, h http.Handler, method, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func publishProvider(t *testing.T, h http.Handler, cid, providerID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return providerRequest(t, h, http.MethodPut,
		"/v1/providers/"+cid+"/"+providerID, body, "application/json")
}

func providerBody(addresses []string, expiresAt time.Time) string {
	raw, _ := json.Marshal(struct {
		Addresses []string `json:"addresses"`
		ExpiresAt string   `json:"expiresAt"`
	}{Addresses: addresses, ExpiresAt: expiresAt.Format(time.RFC3339)})
	return string(raw)
}

func validProviderBody(now time.Time) string {
	return providerBody(
		[]string{"https://node-1.example/", "https://node-2.example:8443/gw"},
		now.Add(time.Hour),
	)
}

func decodeProviderRecord(t *testing.T, rec *httptest.ResponseRecorder) providerRecordResponse {
	t.Helper()
	var resp providerRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("provider response is not JSON: %v", err)
	}
	return resp
}

func decodeProviderList(t *testing.T, rec *httptest.ResponseRecorder) providerListResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp providerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("provider list is not JSON: %v", err)
	}
	return resp
}

func TestProviderPublishListUpdateDeleteLifecycle(t *testing.T) {
	h := Handler()
	now := time.Now()

	rec := publishProvider(t, h, testCID, "node-a", validProviderBody(now))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first publish status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	created := decodeProviderRecord(t, rec)
	if created.ProviderID != "node-a" || len(created.Addresses) != 2 {
		t.Fatalf("unexpected created record: %+v", created)
	}
	if created.ExpiresAt.IsZero() {
		t.Fatalf("expiresAt not echoed: %+v", created)
	}

	// Publishing for a CID that is not stored locally still succeeds.
	otherCID := "sha256:" + strings.Repeat("ab", 32)
	if rec := publishProvider(t, h, otherCID, "node-b",
		providerBody([]string{"http://other.example/"}, now.Add(2*time.Hour))); rec.Code != http.StatusCreated {
		t.Fatalf("publish for unknown CID status = %d, want 201", rec.Code)
	}

	// An update of a valid record returns 200 with the new record.
	updated := providerBody([]string{"https://node-1.example/v2"}, now.Add(3*time.Hour))
	rec = publishProvider(t, h, testCID, "node-a", updated)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	upd := decodeProviderRecord(t, rec)
	if len(upd.Addresses) != 1 || upd.Addresses[0] != "https://node-1.example/v2" {
		t.Fatalf("update did not replace addresses: %+v", upd)
	}

	// A second provider appears, and the listing is sorted by providerId.
	if rec := publishProvider(t, h, testCID, "node-c",
		providerBody([]string{"http://c.example/"}, now.Add(time.Hour))); rec.Code != http.StatusCreated {
		t.Fatalf("second provider status = %d, want 201", rec.Code)
	}
	list := decodeProviderList(t, providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", ""))
	if list.CID != testCID || len(list.Providers) != 2 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list.Providers[0].ProviderID != "node-a" || list.Providers[1].ProviderID != "node-c" {
		t.Fatalf("providers not sorted by id: %+v", list.Providers)
	}
	ids := []string{list.Providers[0].ProviderID, list.Providers[1].ProviderID}
	if ids[0] >= ids[1] {
		t.Fatalf("providers not sorted: %v", ids)
	}

	// Delete one provider: 204, then it disappears.
	rec = providerRequest(t, h, http.MethodDelete, "/v1/providers/"+testCID+"/node-c", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	list = decodeProviderList(t, providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", ""))
	if len(list.Providers) != 1 || list.Providers[0].ProviderID != "node-a" {
		t.Fatalf("deleted provider still listed: %+v", list)
	}

	// Deleting again reports provider_not_found.
	rec = providerRequest(t, h, http.MethodDelete, "/v1/providers/"+testCID+"/node-c", "", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "provider_not_found") {
		t.Fatalf("second delete = %d %s, want 404 provider_not_found", rec.Code, rec.Body.String())
	}
}

func TestProviderListEmptyUnknownCID(t *testing.T) {
	h := Handler()
	// Valid format but no records (and no local object): 200 with an empty
	// array, never null.
	rec := providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != `{"cid":"`+testCID+`","providers":[]}` {
		t.Fatalf("empty list body = %q", body)
	}
}

func TestProviderDirectoryExpiration(t *testing.T) {
	d := newProviderDirectory()
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	expiry := base.Add(time.Minute)

	if created := d.put(testCID, "p1", []string{"https://a/"}, expiry, base); !created {
		t.Fatalf("first put created = false, want true")
	}
	if created := d.put(testCID, "p1", []string{"https://a/"}, expiry, base.Add(30*time.Second)); created {
		t.Fatalf("valid update created = true, want false")
	}
	entries := d.list(testCID, base.Add(30*time.Second))
	if len(entries) != 1 {
		t.Fatalf("valid entries = %d, want 1", len(entries))
	}
	// At and after the expiry instant the record must not resurface.
	if entries := d.list(testCID, expiry); len(entries) != 0 {
		t.Fatalf("expired entries listed: %+v", entries)
	}
	if entries := d.list(testCID, expiry.Add(time.Second)); len(entries) != 0 {
		t.Fatalf("expired entries listed after expiry: %+v", entries)
	}
	if d.remove(testCID, "p1", expiry) {
		t.Fatalf("remove of expired record reported present")
	}
	// Replacing an expired record is a first publication again.
	if created := d.put(testCID, "p1", []string{"https://b/"}, expiry.Add(time.Hour), expiry.Add(time.Second)); !created {
		t.Fatalf("republish after expiry created = false, want true")
	}
	if entries := d.list(testCID, expiry.Add(2*time.Second)); len(entries) != 1 || entries[0].addresses[0] != "https://b/" {
		t.Fatalf("republished record wrong: %+v", entries)
	}

	// Listing a different CID never leaks records.
	if entries := d.list("sha256:"+strings.Repeat("ff", 32), base); len(entries) != 0 {
		t.Fatalf("unrelated cid listing non-empty: %+v", entries)
	}
}

func TestProviderHTTPExpiration(t *testing.T) {
	h := Handler()
	// Short real TTL: after expiry the record vanishes and may be
	// republished as a first publication.
	body := providerBody([]string{"https://short/"}, time.Now().Add(time.Second))
	if rec := publishProvider(t, h, testCID, "short", body); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, want 201", rec.Code)
	}
	time.Sleep(1100 * time.Millisecond)
	if list := decodeProviderList(t, providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", "")); len(list.Providers) != 0 {
		t.Fatalf("expired record still listed: %+v", list.Providers)
	}
	rec := providerRequest(t, h, http.MethodDelete, "/v1/providers/"+testCID+"/short", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete expired = %d, want 404", rec.Code)
	}
	body = providerBody([]string{"https://short/"}, time.Now().Add(time.Hour))
	if rec := publishProvider(t, h, testCID, "short", body); rec.Code != http.StatusCreated {
		t.Fatalf("republish after expiry = %d, want 201", rec.Code)
	}
}

func TestProviderInvalidCIDAndProvider(t *testing.T) {
	h := Handler()
	goodBody := validProviderBody(time.Now())
	cases := []struct {
		method string
		path   string
		code   string
		status int
	}{
		{http.MethodPut, "/v1/providers/not-a-cid/node", "invalid_cid", http.StatusBadRequest},
		{http.MethodGet, "/v1/providers/not-a-cid", "invalid_cid", http.StatusBadRequest},
		{http.MethodDelete, "/v1/providers/not-a-cid/node", "invalid_cid", http.StatusBadRequest},
		{http.MethodPut, "/v1/providers/" + testCID + "/UPPER", "invalid_provider", http.StatusBadRequest},
		{http.MethodPut, "/v1/providers/" + testCID + "/-leading", "invalid_provider", http.StatusBadRequest},
		{http.MethodPut, "/v1/providers/" + testCID + "/trailing-", "invalid_provider", http.StatusBadRequest},
		{http.MethodDelete, "/v1/providers/" + testCID + "/under_score", "invalid_provider", http.StatusBadRequest},
	}
	for _, tc := range cases {
		body := ""
		ct := ""
		if tc.method == http.MethodPut {
			body, ct = goodBody, "application/json"
		}
		rec := providerRequest(t, h, tc.method, tc.path, body, ct)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Errorf("%s %s = %d %s, want %d %s", tc.method, tc.path, rec.Code, rec.Body.String(), tc.status, tc.code)
		}
	}

	// Provider IDs at the length boundaries are accepted shape-wise.
	longID := strings.Repeat("a", 64)
	if rec := publishProvider(t, h, testCID, longID, validProviderBody(time.Now())); rec.Code != http.StatusCreated {
		t.Fatalf("64-char id status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := publishProvider(t, h, testCID, strings.Repeat("a", 65), goodBody); rec.Code != http.StatusBadRequest {
		t.Fatalf("65-char id status = %d, want 400", rec.Code)
	}
}

func TestProviderInvalidRequestBodies(t *testing.T) {
	h := Handler()
	path := "/v1/providers/" + testCID + "/node"
	cases := map[string]string{
		"empty body":             "",
		"not json":               "{",
		"top level array":        `["https://a/"]`,
		"top level string":       `"hello"`,
		"missing addresses":      `{"expiresAt":"2999-01-01T00:00:00Z"}`,
		"missing expiresAt":      `{"addresses":["https://a/"]}`,
		"unknown field":          `{"addresses":["https://a/"],"expiresAt":"2999-01-01T00:00:00Z","extra":1}`,
		"duplicate field":        `{"addresses":["https://a/"],"addresses":["https://b/"],"expiresAt":"2999-01-01T00:00:00Z"}`,
		"null addresses":         `{"addresses":null,"expiresAt":"2999-01-01T00:00:00Z"}`,
		"null expiresAt":         `{"addresses":["https://a/"],"expiresAt":null}`,
		"addresses wrong type":   `{"addresses":"https://a/","expiresAt":"2999-01-01T00:00:00Z"}`,
		"expiresAt wrong type":   `{"addresses":["https://a/"],"expiresAt":123}`,
		"address element number": `{"addresses":[1],"expiresAt":"2999-01-01T00:00:00Z"}`,
		"address element null":   `{"addresses":[null],"expiresAt":"2999-01-01T00:00:00Z"}`,
		"trailing content":       `{"addresses":["https://a/"],"expiresAt":"2999-01-01T00:00:00Z"} junk`,
		"two json values":        `{"addresses":["https://a/"],"expiresAt":"2999-01-01T00:00:00Z"}{}`,
	}
	for name, body := range cases {
		rec := providerRequest(t, h, http.MethodPut, path, body, "application/json")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
			t.Errorf("%s: got %d %s, want 400 invalid_request", name, rec.Code, rec.Body.String())
		}
	}
}

func TestProviderMediaType(t *testing.T) {
	h := Handler()
	path := "/v1/providers/" + testCID + "/node"
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		rec := providerRequest(t, h, http.MethodPut, path, validProviderBody(time.Now()), ct)
		if rec.Code != http.StatusUnsupportedMediaType ||
			!strings.Contains(rec.Body.String(), "unsupported_media_type") {
			t.Errorf("Content-Type %q: got %d %s, want 415 unsupported_media_type",
				ct, rec.Code, rec.Body.String())
		}
	}
	// Parameters are tolerated.
	rec := providerRequest(t, h, http.MethodPut, path, validProviderBody(time.Now()),
		"application/json; charset=utf-8")
	if rec.Code != http.StatusCreated {
		t.Fatalf("application/json with parameters = %d, want 201", rec.Code)
	}
}

func TestProviderInvalidAddresses(t *testing.T) {
	h := Handler()
	path := "/v1/providers/" + testCID + "/node"
	goodExpiry := `"expiresAt":"2999-01-01T00:00:00Z"}`
	cases := map[string]string{
		"empty list":        `{"addresses":[],` + goodExpiry,
		"17 addresses":      `{"addresses":["https://h01/","https://h02/","https://h03/","https://h04/","https://h05/","https://h06/","https://h07/","https://h08/","https://h09/","https://h10/","https://h11/","https://h12/","https://h13/","https://h14/","https://h15/","https://h16/","https://h17/"],` + goodExpiry,
		"duplicate":         `{"addresses":["https://a/","https://a/"],` + goodExpiry,
		"unsorted":          `{"addresses":["https://b/","https://a/"],` + goodExpiry,
		"ftp scheme":        `{"addresses":["ftp://host/"],` + goodExpiry,
		"relative":          `{"addresses":["/just/a/path"],` + goodExpiry,
		"protocol-relative": `{"addresses":["//host/path"],` + goodExpiry,
		"userinfo":          `{"addresses":["http://user:pass@host/"],` + goodExpiry,
		"empty host":        `{"addresses":["http:///path"],` + goodExpiry,
		"fragment":          `{"addresses":["https://host/path#frag"],` + goodExpiry,
		"empty fragment":    `{"addresses":["https://host/#"],` + goodExpiry,
		"empty string":      `{"addresses":[""],` + goodExpiry,
		"one bad of two":    `{"addresses":["https://good/","mailto:x@y"],` + goodExpiry,
	}
	for name, body := range cases {
		rec := providerRequest(t, h, http.MethodPut, path, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(rec.Body.String(), "invalid_address") {
			t.Errorf("%s: got %d %s, want 422 invalid_address", name, rec.Code, rec.Body.String())
		}
	}

	// 16 distinct, sorted URLs are accepted; http with a port and query is
	// fine, and a trailing slash is not required.
	good := []string{"http://a", "https://b.example:8443/gw?x=1"}
	rec := publishProvider(t, h, testCID, "goodaddrs", providerBody(good, time.Now().Add(time.Hour)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid addresses status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestProviderInvalidExpiration(t *testing.T) {
	h := Handler()
	path := "/v1/providers/" + testCID + "/node"
	now := time.Now()
	cases := map[string]string{
		"not a timestamp":  `{"addresses":["https://a/"],"expiresAt":"soon"}`,
		"date only":        `{"addresses":["https://a/"],"expiresAt":"2026-10-03"}`,
		"in the past":      providerBody([]string{"https://a/"}, now.Add(-time.Minute)),
		"one sec past 24h": providerBody([]string{"https://a/"}, now.Add(24*time.Hour+time.Second)),
	}
	for name, body := range cases {
		rec := providerRequest(t, h, http.MethodPut, path, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(rec.Body.String(), "invalid_expiration") {
			t.Errorf("%s: got %d %s, want 422 invalid_expiration", name, rec.Code, rec.Body.String())
		}
	}
	// Just inside the 24-hour ceiling is accepted.
	body := providerBody([]string{"https://a/"}, now.Add(24*time.Hour-time.Second))
	if rec := providerRequest(t, h, http.MethodPut, path, body, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("near-ceiling expiration = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestProviderMethodNotAllowed(t *testing.T) {
	h := Handler()
	rec := providerRequest(t, h, http.MethodPost, "/v1/providers/"+testCID, "", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST collection item = %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Fatalf("missing method_not_allowed body: %s", rec.Body.String())
	}
	rec = providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID+"/node", "", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "PUT, DELETE" {
		t.Fatalf("GET provider = %d Allow=%q, want 405 'PUT, DELETE'", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestProviderConcurrentFirstPublish(t *testing.T) {
	h := Handler()
	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, n)
	body := validProviderBody(time.Now())
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			statuses[i] = publishProvider(t, h, testCID, "race-node", body).Code
		}(i)
	}
	close(start)
	wg.Wait()
	created, updated := 0, 0
	for _, code := range statuses {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			updated++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || updated != n-1 {
		t.Fatalf("created = %d, updated = %d, want exactly 1 created", created, updated)
	}
	list := decodeProviderList(t, providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", ""))
	if len(list.Providers) != 1 || list.Providers[0].ProviderID != "race-node" {
		t.Fatalf("listing after race: %+v", list)
	}
}

func TestProviderConcurrentAccessSnapshot(t *testing.T) {
	// Under -race this smoke-tests that listing always observes complete
	// records while records churn.
	h := Handler()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("node-%d-%d", g, i%8)
				body := providerBody([]string{"https://x/"}, time.Now().Add(time.Hour))
				publishProvider(t, h, testCID, id, body)
			}
		}(g)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := providerRequest(t, h, http.MethodGet, "/v1/providers/"+testCID, "", "")
				if rec.Code != http.StatusOK {
					t.Errorf("list status = %d", rec.Code)
					return
				}
				var resp providerListResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Errorf("list not JSON: %v", err)
					return
				}
				for _, p := range resp.Providers {
					if p.ProviderID == "" || len(p.Addresses) != 1 || p.ExpiresAt.IsZero() {
						t.Errorf("incomplete snapshot record: %+v", p)
						return
					}
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestProviderDoesNotAffectStorage(t *testing.T) {
	h := Handler()
	before := storageStatsResponse(t, h)
	if rec := publishProvider(t, h, testCID, "node", validProviderBody(time.Now())); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, want 201", rec.Code)
	}
	after := storageStatsResponse(t, h)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("storage stats changed: before=%+v after=%+v", before, after)
	}
}

func storageStatsResponse(t *testing.T, h http.Handler) map[string]int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d", rec.Code)
	}
	var resp map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("stats not JSON: %v", err)
	}
	return resp
}
