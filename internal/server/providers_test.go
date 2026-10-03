package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func providerCID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return cidPrefix + hex.EncodeToString(sum[:])
}

func putProvider(t *testing.T, h http.Handler, cid, providerID, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/providers/"+cid+"/"+providerID, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getProviders(t *testing.T, h http.Handler, cid string) providerListResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/providers/"+cid, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET providers status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp providerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("provider list is not JSON: %v", err)
	}
	return resp
}

func validProviderBody(expiresIn time.Duration) string {
	expiry := time.Now().Add(expiresIn).UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(`{"addresses":["https://b.example/","http://a.example:8080/p"],"expiresAt":%q}`, expiry)
}

func TestProviderPublishUpdateGetDelete(t *testing.T) {
	h := Handler()
	cid := providerCID("no local object needed")

	// Publishing works without the object being stored locally.
	rec := putProvider(t, h, cid, "node-1", validProviderBody(time.Hour), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first publish status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created providerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("publish response is not JSON: %v", err)
	}
	if created.ProviderID != "node-1" || len(created.Addresses) != 2 ||
		created.Addresses[0] != "http://a.example:8080/p" || created.Addresses[1] != "https://b.example/" {
		t.Fatalf("addresses must be returned sorted: %+v", created.Addresses)
	}

	// Updating the still-valid record returns 200 and replaces addresses.
	body := fmt.Sprintf(`{"addresses":["https://c.example/"],"expiresAt":%q}`,
		time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339Nano))
	rec = putProvider(t, h, cid, "node-1", body, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}

	// A second provider is listed too, sorted by providerId.
	if rec := putProvider(t, h, cid, "node-2", validProviderBody(time.Hour), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("second provider status = %d, want %d", rec.Code, http.StatusCreated)
	}
	list := getProviders(t, h, cid)
	if list.CID != cid || len(list.Providers) != 2 {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list.Providers[0].ProviderID != "node-1" || list.Providers[1].ProviderID != "node-2" {
		t.Fatalf("providers not sorted by id: %+v", list.Providers)
	}
	if got := list.Providers[0].Addresses; len(got) != 1 || got[0] != "https://c.example/" {
		t.Fatalf("updated addresses not reflected: %+v", got)
	}

	// Unknown cid is a successful, empty listing.
	emptyRec := httptest.NewRecorder()
	h.ServeHTTP(emptyRec, httptest.NewRequest(http.MethodGet, "/v1/providers/"+providerCID("unknown"), nil))
	if emptyRec.Code != http.StatusOK || !strings.Contains(emptyRec.Body.String(), `"providers":[]`) {
		t.Fatalf("unknown cid should be 200 with empty providers: %d %s", emptyRec.Code, emptyRec.Body.String())
	}

	// Delete one provider; the other remains. A second delete is a 404.
	del := httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/node-1", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, del)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/node-1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "provider_not_found")
	if list := getProviders(t, h, cid); len(list.Providers) != 1 || list.Providers[0].ProviderID != "node-2" {
		t.Fatalf("unexpected providers after delete: %+v", list.Providers)
	}
}

func TestProviderExpiry(t *testing.T) {
	h := Handler()
	cid := providerCID("expiry")
	expiry := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"addresses":["http://x.example/"],"expiresAt":%q}`, expiry)
	if rec := putProvider(t, h, cid, "ephemeral", body, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d", rec.Code)
	}
	if list := getProviders(t, h, cid); len(list.Providers) != 1 {
		t.Fatalf("provider should be listed before expiry: %+v", list.Providers)
	}

	time.Sleep(300 * time.Millisecond)

	if list := getProviders(t, h, cid); len(list.Providers) != 0 {
		t.Fatalf("expired provider must not be listed: %+v", list.Providers)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/ephemeral", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete expired status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "provider_not_found")
	// Republishing after expiry is a first publication again.
	if rec := putProvider(t, h, cid, "ephemeral", validProviderBody(time.Hour), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("republish after expiry status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestProviderValidationErrors(t *testing.T) {
	h := Handler()
	cid := providerCID("validation")

	// Media type missing or wrong.
	for _, ct := range []string{"", "text/plain", "application/octet-stream"} {
		rec := putProvider(t, h, cid, "node", validProviderBody(time.Hour), ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Structural/type problems.
	for _, body := range []string{
		`{`,
		`{}`,
		`{"addresses":["http://a.example/"]}`,
		`{"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":null,"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://a.example/"],"expiresAt":null}`,
		`{"addresses":"http://a.example/","expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":[1],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":[null],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://a.example/"],"expiresAt":123}`,
		`{"addresses":["http://a.example/"],"expiresAt":"2030-01-01T00:00:00Z","extra":1}`,
		`{"addresses":["http://a.example/"],"addresses":["http://b.example/"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://a.example/"],"expiresAt":"2030-01-01T00:00:00Z"} trailing`,
		`[1]`,
	} {
		rec := putProvider(t, h, cid, "node", body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// Non-compliant addresses.
	for _, body := range []string{
		`{"addresses":[],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://a.example/","http://a.example/"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["/relative/path"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["ftp://a.example/"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://user:pass@a.example/"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http://a.example/#frag"],"expiresAt":"2030-01-01T00:00:00Z"}`,
		`{"addresses":["http:///path"],"expiresAt":"2030-01-01T00:00:00Z"}`,
	} {
		rec := putProvider(t, h, cid, "node", body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_address")
	}

	// Seventeen distinct addresses exceed the cap of 16.
	var urls [17]string
	for i := range urls {
		urls[i] = fmt.Sprintf("http://h%d.example/", i)
	}
	tooMany, _ := json.Marshal(map[string]any{"addresses": urls[:], "expiresAt": "2030-01-01T00:00:00Z"})
	if rec := putProvider(t, h, cid, "node", string(tooMany), "application/json"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("17 addresses: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}

	// Expiration problems.
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	future25h := time.Now().Add(25 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, body := range []string{
		`{"addresses":["http://a.example/"],"expiresAt":"not-a-time"}`,
		fmt.Sprintf(`{"addresses":["http://a.example/"],"expiresAt":%q}`, past),
		fmt.Sprintf(`{"addresses":["http://a.example/"],"expiresAt":%q}`, future25h),
	} {
		rec := putProvider(t, h, cid, "node", body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_expiration")
	}

	// Exactly the 24h ceiling is accepted.
	atCeiling := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"addresses":["http://a.example/"],"expiresAt":%q}`, atCeiling)
	if rec := putProvider(t, h, cid, "ceiling-node", body, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("24h expiry status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Invalid cid and providerId shapes, on every method.
	for _, providerID := range []string{"UPPER", "-lead", "trail-", "under_score", "a.b", strings.Repeat("a", 65)} {
		rec := putProvider(t, h, "not-a-cid", providerID, validProviderBody(time.Hour), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cid check should win for %q: status = %d", providerID, rec.Code)
		}
		assertErrorCode(t, rec, "invalid_cid")
		rec = putProvider(t, h, cid, providerID, validProviderBody(time.Hour), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("providerId %q: status = %d, want %d", providerID, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_provider")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/providers/not-a-cid", nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET invalid cid status = %d", rec.Code)
		}
		assertErrorCode(t, rec, "invalid_cid")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/"+providerID, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("DELETE providerId %q: status = %d", providerID, rec.Code)
		}
		assertErrorCode(t, rec, "invalid_provider")
	}
}

func TestProviderMethodNotAllowed(t *testing.T) {
	h := Handler()
	cid := providerCID("methods")
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodPost, "/v1/providers/" + cid, http.MethodGet},
		{http.MethodPut, "/v1/providers/" + cid, http.MethodGet},
		{http.MethodDelete, "/v1/providers/" + cid, http.MethodGet},
		{http.MethodGet, "/v1/providers/" + cid + "/node", "PUT, DELETE"},
		{http.MethodPost, "/v1/providers/" + cid + "/node", "PUT, DELETE"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, allow, tc.allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestProviderConcurrentFirstPublishSingle201(t *testing.T) {
	h := Handler()
	cid := providerCID("race")
	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := putProvider(t, h, cid, "race-node", validProviderBody(time.Hour), "application/json")
			mu.Lock()
			statuses[i] = rec.Code
			mu.Unlock()
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
			t.Fatalf("unexpected publish status %d", code)
		}
	}
	if created != 1 || updated != n-1 {
		t.Fatalf("got %d created and %d updated, want 1 and %d", created, updated, n-1)
	}

	// Concurrent deletes: exactly one sees the valid record.
	var delCodes [n]int
	start = make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/providers/"+cid+"/race-node", nil))
			delCodes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()
	gone, missing := 0, 0
	for _, code := range delCodes {
		if code == http.StatusNoContent {
			gone++
		} else if code == http.StatusNotFound {
			missing++
		} else {
			t.Fatalf("unexpected delete status %d", code)
		}
	}
	if gone != 1 || missing != n-1 {
		t.Fatalf("got %d x 204 and %d x 404, want 1 and %d", gone, missing, n-1)
	}
}

func TestProviderSnapshotAndExpiryUnderConcurrency(t *testing.T) {
	h := Handler()
	cid := providerCID("snapshot")
	// A record that expires mid-run.
	short := fmt.Sprintf(`{"addresses":["http://short.example/"],"expiresAt":%q}`,
		time.Now().Add(150*time.Millisecond).UTC().Format(time.RFC3339Nano))
	if rec := putProvider(t, h, cid, "short", short, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("seed publish status = %d", rec.Code)
	}

	const rounds, readers, writers = 100, 8, 4
	var wg sync.WaitGroup
	// Bounded iterations with explicit yields keep the run deterministic
	// and scheduler-friendly under the race detector.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < rounds; k++ {
				list := getProviders(t, h, cid)
				for _, p := range list.Providers {
					if len(p.Addresses) < 1 || len(p.Addresses) > 16 {
						t.Errorf("inconsistent snapshot for %q: %+v", p.ProviderID, p.Addresses)
						return
					}
				}
				runtime.Gosched()
			}
		}()
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("writer-%d", i)
			for k := 0; k < rounds; k++ {
				putProvider(t, h, cid, id, validProviderBody(time.Hour), "application/json")
				runtime.Gosched()
			}
		}(i)
	}
	wg.Wait()

	// The short-lived record expired during the run; give it a moment, then
	// hammer listings to prove it never reappears.
	time.Sleep(300 * time.Millisecond)
	for k := 0; k < 50; k++ {
		list := getProviders(t, h, cid)
		for _, p := range list.Providers {
			if p.ProviderID == "short" {
				t.Fatalf("expired record reappeared: %+v", p)
			}
		}
		runtime.Gosched()
	}
}

func TestProvidersIndependentOfObjectsPinsAndGC(t *testing.T) {
	h := Handler()
	cid := providerCID("independent")
	if rec := putProvider(t, h, cid, "far-node", validProviderBody(time.Hour), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("publish without local object status = %d", rec.Code)
	}

	// Garbage-collecting everything leaves the directory untouched.
	if _, result := runGC(t, h, ""); result.Objects != 0 {
		t.Fatalf("no objects should exist yet: %+v", result)
	}
	list := getProviders(t, h, cid)
	if len(list.Providers) != 1 || list.Providers[0].ProviderID != "far-node" {
		t.Fatalf("gc must not touch providers: %+v", list.Providers)
	}

	// Providers never enter storage statistics.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	var stats statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("stats not JSON: %v", err)
	}
	if stats.Objects != 0 || stats.Blocks != 0 || stats.LogicalBytes != 0 || stats.StoredBytes != 0 {
		t.Fatalf("provider records must not count in stats: %+v", stats)
	}

	// Providers never appear in the pin listing.
	if pins := listPins(t, h); len(pins.Pins) != 0 {
		t.Fatalf("provider records must not appear as pins: %+v", pins.Pins)
	}
}
