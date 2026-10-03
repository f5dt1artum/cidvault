package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func retrievalRequest(t *testing.T, h http.Handler, method, cid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/retrievals/"+cid, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeRetrieval(t *testing.T, rec *httptest.ResponseRecorder) retrievalResponse {
	t.Helper()
	var resp retrievalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("retrieval response is not JSON: %v", err)
	}
	return resp
}

func uploadObject(t *testing.T, h http.Handler, body []byte) objectResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/objects", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var resp objectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("upload response is not JSON: %v", err)
	}
	return resp
}

func TestRetrievalMethodAndCIDValidation(t *testing.T) {
	h := Handler()

	rec := retrievalRequest(t, h, http.MethodPost, "not-a-cid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CID status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_cid")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := retrievalRequest(t, h, method, testCID)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s Allow = %q, want %q", method, allow, http.MethodPost)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestRetrievalExistingObjectSkipsNetwork(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("already here"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("provider contacted although the object is stored locally")
	}))
	defer srv.Close()
	publishProvider(t, h, up.CID, "node-a", providerBody([]string{srv.URL}, time.Now().Add(time.Hour)))

	rec := retrievalRequest(t, h, http.MethodPost, up.CID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if resp.Created {
		t.Fatalf("created = true, want false: %+v", resp)
	}
	if resp.ProviderID != nil || resp.Address != nil {
		t.Fatalf("providerId/address = %v/%v, want both null", resp.ProviderID, resp.Address)
	}
	if resp.CID != up.CID || resp.Size != up.Size || resp.ChunkSize != up.ChunkSize ||
		!reflect.DeepEqual(resp.Chunks, up.Chunks) {
		t.Fatalf("response %+v does not match upload %+v", resp, up)
	}
}

func TestRetrievalNoProvider(t *testing.T) {
	h := Handler()
	rec := retrievalRequest(t, h, http.MethodPost, testCID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "no_provider")

	// A provider whose record was deleted is not a valid provider either.
	publishProvider(t, h, testCID, "node-a", validProviderBody(time.Now()))
	req := httptest.NewRequest(http.MethodDelete, "/v1/providers/"+testCID+"/node-a", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete provider status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = retrievalRequest(t, h, http.MethodPost, testCID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status after delete = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "no_provider")
}

func TestRetrievalFetchesAndStores(t *testing.T) {
	h := Handler()
	body := []byte(strings.Repeat("cidvault-", 150000)) // spans two chunks
	want := newObject(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("provider request path = %q, want the published URL unchanged", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishProvider(t, h, want.cid, "node-a",
		providerBody([]string{srv.URL}, time.Now().Add(time.Hour)))

	rec := retrievalRequest(t, h, http.MethodPost, want.cid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if !resp.Created {
		t.Fatalf("created = false, want true: %+v", resp)
	}
	if resp.ProviderID == nil || *resp.ProviderID != "node-a" {
		t.Fatalf("providerId = %v, want node-a", resp.ProviderID)
	}
	if resp.Address == nil || *resp.Address != srv.URL {
		t.Fatalf("address = %v, want %s", resp.Address, srv.URL)
	}
	if resp.CID != want.cid || resp.Size != len(body) || resp.ChunkSize != ChunkSize ||
		!reflect.DeepEqual(resp.Chunks, want.cids) {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// The object now reads back exactly as a direct upload.
	req := httptest.NewRequest(http.MethodGet, "/v1/objects/"+want.cid, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("GET object status = %d, body mismatch", rec.Code)
	}

	// A second retrieval is local: 200, created=false, null provenance.
	rec = retrievalRequest(t, h, http.MethodPost, want.cid)
	if rec.Code != http.StatusOK {
		t.Fatalf("second retrieval status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp = decodeRetrieval(t, rec)
	if resp.Created || resp.ProviderID != nil || resp.Address != nil {
		t.Fatalf("second retrieval = %+v, want created=false with null provenance", resp)
	}

	// Importing does not pin, and statistics match a direct upload.
	req = httptest.NewRequest(http.MethodGet, "/v1/pins", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var pins struct {
		Pins []any `json:"pins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pins); err != nil || len(pins.Pins) != 0 {
		t.Fatalf("pins = %s, want empty", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if st.Objects != 1 || st.LogicalBytes != len(body) || st.Blocks != 2 || st.StoredBytes != len(body) {
		t.Fatalf("stats = %+v, want one object with two blocks", st)
	}
}

func TestRetrievalAddressFallback(t *testing.T) {
	h := Handler()
	body := []byte("the good bytes")
	want := newObject(body)

	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/a":
			// Redirects are not followed, so this address fails.
			http.Redirect(w, r, "/d", http.StatusFound)
		case "/b":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(body)
		case "/c":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("bytes that hash to a different CID"))
		case "/d":
			w.Header().Set("Content-Type", "application/octet-stream; charset=binary")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	addresses := []string{srv.URL + "/a", srv.URL + "/b", srv.URL + "/c", srv.URL + "/d"}
	publishProvider(t, h, want.cid, "node-a", providerBody(addresses, time.Now().Add(time.Hour)))

	rec := retrievalRequest(t, h, http.MethodPost, want.cid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if resp.Address == nil || *resp.Address != srv.URL+"/d" {
		t.Fatalf("address = %v, want %s/d", resp.Address, srv.URL)
	}
	mu.Lock()
	got := append([]string{}, hits...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"/a", "/b", "/c", "/d"}) {
		t.Fatalf("attempt order = %v, want /a /b /c /d", got)
	}
}

func TestRetrievalAllAddressesFail(t *testing.T) {
	h := Handler()
	want := newObject([]byte("never delivered"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	publishProvider(t, h, want.cid, "node-b", providerBody([]string{srv.URL}, time.Now().Add(time.Hour)))
	publishProvider(t, h, want.cid, "node-a",
		providerBody([]string{srv.URL + "/gone"}, time.Now().Add(time.Hour)))

	rec := retrievalRequest(t, h, http.MethodPost, want.cid)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	assertErrorCode(t, rec, "retrieval_failed")

	// The failed attempt left no object, blocks, pins or statistics behind.
	req := httptest.NewRequest(http.MethodGet, "/v1/objects/"+want.cid, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if st.Objects != 0 || st.LogicalBytes != 0 || st.Blocks != 0 || st.StoredBytes != 0 {
		t.Fatalf("stats = %+v, want all zero", st)
	}
}

func TestRetrievalConcurrentSingleCreate(t *testing.T) {
	h := Handler()
	body := []byte("fetched exactly once")
	want := newObject(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishProvider(t, h, want.cid, "node-a", providerBody([]string{srv.URL}, time.Now().Add(time.Hour)))

	const n = 8
	var wg sync.WaitGroup
	created := make([]bool, n)
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := retrievalRequest(t, h, http.MethodPost, want.cid)
			codes[i] = rec.Code
			created[i] = decodeRetrieval(t, rec).Created
		}()
	}
	wg.Wait()

	ones := 0
	for i := range n {
		if created[i] {
			ones++
			if codes[i] != http.StatusCreated {
				t.Fatalf("created response status = %d, want %d", codes[i], http.StatusCreated)
			}
		} else if codes[i] != http.StatusOK {
			t.Fatalf("duplicate response status = %d, want %d", codes[i], http.StatusOK)
		}
	}
	if ones != 1 {
		t.Fatalf("%d responses report created, want exactly 1", ones)
	}
}
