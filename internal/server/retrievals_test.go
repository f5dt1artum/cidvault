package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// retrievalBodyResponse decodes the full retrieval response including the
// nullable provider fields.
type retrievalBodyResponse struct {
	CID        string   `json:"cid"`
	Size       int      `json:"size"`
	ChunkSize  int      `json:"chunkSize"`
	Chunks     []string `json:"chunks"`
	Created    bool     `json:"created"`
	ProviderID *string  `json:"providerId"`
	Address    *string  `json:"address"`
}

func retrievalRequest(t *testing.T, h http.Handler, cid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/retrievals/"+cid, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeRetrieval(t *testing.T, rec *httptest.ResponseRecorder) retrievalBodyResponse {
	t.Helper()
	var resp retrievalBodyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("retrieval response is not JSON: %v: %s", err, rec.Body.String())
	}
	return resp
}

func cidForBody(body []byte) string {
	obj := buildObject(body)
	return obj.cid
}

// publishRetrievalProvider registers one provider record for cid with the
// given addresses (sorted first, as publication requires).
func publishRetrievalProvider(t *testing.T, h http.Handler, cid, providerID string, addresses []string, expiry time.Time) {
	t.Helper()
	sorted := append([]string(nil), addresses...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	rec := providerRequest(t, h, http.MethodPut,
		"/v1/providers/"+cid+"/"+providerID,
		providerBody(sorted, expiry), "application/json")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("publish provider status = %d: %s", rec.Code, rec.Body.String())
	}
}

// octetServer returns the given bytes as application/octet-stream 200.
func octetServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
}

func statsSnapshot(t *testing.T, h http.Handler) storageStats {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d", rec.Code)
	}
	var resp struct {
		Objects      int `json:"objects"`
		LogicalBytes int `json:"logicalBytes"`
		Blocks       int `json:"blocks"`
		StoredBytes  int `json:"storedBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("stats not JSON: %v", err)
	}
	return storageStats{
		objects:      resp.Objects,
		logicalBytes: resp.LogicalBytes,
		blocks:       resp.Blocks,
		storedBytes:  resp.StoredBytes,
	}
}

func TestRetrievalInvalidCIDAndMethod(t *testing.T) {
	h := Handler()
	rec := retrievalRequest(t, h, "not-a-cid")
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("invalid cid: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/retrievals/"+testCID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

func TestRetrievalExistingObjectSkipsNetwork(t *testing.T) {
	h := Handler()
	body := []byte("already here")
	obj := buildObject(body)

	up := httptest.NewRequest(http.MethodPost, "/v1/objects", strings.NewReader(string(body)))
	up.Header.Set("Content-Type", "application/octet-stream")
	upRec := httptest.NewRecorder()
	h.ServeHTTP(upRec, up)
	if upRec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", upRec.Code)
	}
	cid := cidForBody(body)

	// Register a provider that must never be contacted: no server listens.
	now := time.Now()
	publishRetrievalProvider(t, h, cid, "ghost",
		[]string{"http://127.0.0.1:1/unreachable"}, now.Add(time.Hour))

	rec := retrievalRequest(t, h, cid)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if resp.Created || resp.ProviderID != nil || resp.Address != nil {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.CID != cid || resp.Size != len(body) ||
		resp.ChunkSize != ChunkSize || len(resp.Chunks) != 1 || resp.Chunks[0] != obj.cids[0] {
		t.Fatalf("response fields disagree with upload response: %+v", resp)
	}
}

func TestRetrievalNoProvider(t *testing.T) {
	h := Handler()
	rec := retrievalRequest(t, h, testCID)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "no_provider" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := statsSnapshot(t, h); got != (storageStats{}) {
		t.Fatalf("stats changed after no_provider: %+v", got)
	}
}

func TestRetrievalSuccessStoresLikeUpload(t *testing.T) {
	h := Handler()
	body := []byte("the quick brown fox jumps over the lazy dog")
	cid := cidForBody(body)
	srv := octetServer(t, body)
	defer srv.Close()
	publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

	before := statsSnapshot(t, h)
	rec := retrievalRequest(t, h, cid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if !resp.Created || resp.ProviderID == nil || *resp.ProviderID != "node-a" ||
		resp.Address == nil || *resp.Address != srv.URL {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.CID != cid || resp.Size != len(body) || resp.ChunkSize != ChunkSize {
		t.Fatalf("manifest fields wrong: %+v", resp)
	}

	// Second retrieval for the same object is the already-present response.
	rec2 := retrievalRequest(t, h, cid)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second retrieval status = %d", rec2.Code)
	}
	resp2 := decodeRetrieval(t, rec2)
	if resp2.Created || resp2.ProviderID != nil || resp2.Address != nil {
		t.Fatalf("second retrieval should be local: %+v", resp2)
	}

	// Object, manifest and blocks read exactly as after a direct upload.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK || getRec.Body.String() != string(body) {
		t.Fatalf("object readback status=%d body=%q", getRec.Code, getRec.Body.String())
	}
	blockReq := httptest.NewRequest(http.MethodGet, "/v1/blocks/"+resp.Chunks[0], nil)
	blockRec := httptest.NewRecorder()
	h.ServeHTTP(blockRec, blockReq)
	if blockRec.Code != http.StatusOK || blockRec.Body.String() != string(body) {
		t.Fatalf("block readback status=%d", blockRec.Code)
	}

	after := statsSnapshot(t, h)
	if after.objects != before.objects+1 || after.logicalBytes != len(body) ||
		after.blocks != 1 || after.storedBytes != len(body) {
		t.Fatalf("stats wrong after retrieval: %+v", after)
	}

	// Retrieval creates no pin: the pin list is empty and GC removes it.
	pinsReq := httptest.NewRequest(http.MethodGet, "/v1/pins", nil)
	pinsRec := httptest.NewRecorder()
	h.ServeHTTP(pinsRec, pinsReq)
	if strings.TrimSpace(pinsRec.Body.String()) != `{"pins":[]}` {
		t.Fatalf("retrieval created a pin: %s", pinsRec.Body.String())
	}
	gcReq := httptest.NewRequest(http.MethodPost, "/v1/gc", strings.NewReader(""))
	gcRec := httptest.NewRecorder()
	h.ServeHTTP(gcRec, gcReq)
	if gcRec.Code != http.StatusOK || !strings.Contains(gcRec.Body.String(), cid) {
		t.Fatalf("retrieved object should be collectable: %d %s", gcRec.Code, gcRec.Body.String())
	}
}

func TestRetrievalFailuresLeaveNoTrace(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "status 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "wrong media type",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("nope"))
			},
		},
		{
			name: "cid mismatch",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte("totally different bytes"))
			},
		},
		{
			name: "oversized body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(make([]byte, MaxObjectSize+1))
			},
		},
		{
			name: "redirect not followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/somewhere-else", http.StatusFound)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler()
			body := []byte("payload for " + tc.name)
			cid := cidForBody(body)
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

			rec := retrievalRequest(t, h, cid)
			if rec.Code != http.StatusBadGateway || errorCode(t, rec) != "retrieval_failed" {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := statsSnapshot(t, h); got != (storageStats{}) {
				t.Fatalf("failure left stats changes: %+v", got)
			}
			getReq := httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid, nil)
			getRec := httptest.NewRecorder()
			h.ServeHTTP(getRec, getReq)
			if getRec.Code != http.StatusNotFound {
				t.Fatalf("failure left an object behind: %d", getRec.Code)
			}
		})
	}
}

func TestRetrievalProviderAndAddressOrder(t *testing.T) {
	h := Handler()
	body := []byte("ordered retrieval")
	cid := cidForBody(body)

	var mu sync.Mutex
	var hits []string
	// One server: "/a..." addresses fail, the "/z" address succeeds. Since
	// record addresses are kept in strict lexical order, the failures are
	// guaranteed to be attempted first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/z" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// node-alpha sorts first and only carries failing addresses; node-beta
	// lists one failing address before the working one.
	publishRetrievalProvider(t, h, cid, "node-alpha",
		[]string{srv.URL + "/a1", srv.URL + "/a2"}, time.Now().Add(time.Hour))
	publishRetrievalProvider(t, h, cid, "node-beta",
		[]string{srv.URL + "/b1", srv.URL + "/z"}, time.Now().Add(time.Hour))

	rec := retrievalRequest(t, h, cid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	wantAddr := srv.URL + "/z"
	if resp.ProviderID == nil || *resp.ProviderID != "node-beta" ||
		resp.Address == nil || *resp.Address != wantAddr {
		t.Fatalf("wrong location used: %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/a1", "/a2", "/b1", "/z"}
	if len(hits) != len(want) {
		t.Fatalf("attempt order wrong: %v", hits)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("attempt order wrong: %v", hits)
		}
	}
}

func TestRetrievalUsesAcceptanceSnapshot(t *testing.T) {
	h := Handler()
	body := []byte("snapshot me")
	cid := cidForBody(body)

	proceed := make(chan struct{})
	gotRequest := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the record has been deleted mid-request.
		close(gotRequest)
		<-proceed
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- retrievalRequest(t, h, cid)
	}()

	// Wait for the GET to be in flight using the acceptance snapshot, then
	// delete the record before releasing the response.
	<-gotRequest
	delReq := httptest.NewRequest(http.MethodDelete,
		"/v1/providers/"+cid+"/node-a", nil)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", delRec.Code)
	}
	close(proceed)

	rec := <-result
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if resp.ProviderID == nil || *resp.ProviderID != "node-a" {
		t.Fatalf("snapshot provider not used: %+v", resp)
	}
}

func TestRetrievalConcurrentSingleCreation(t *testing.T) {
	h := Handler()
	body := []byte("concurrent retrieval payload")
	cid := cidForBody(body)

	// A short delay widens the window in which requests race into put.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/retrievals/"+cid, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			statuses[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	created, ok := 0, 0
	for _, st := range statuses {
		switch st {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			ok++
		default:
			t.Fatalf("unexpected status %d", st)
		}
	}
	if created != 1 || ok != n-1 {
		t.Fatalf("want exactly one 201, got %d 201 and %d 200", created, ok)
	}
}

func TestRetrievalDoesNotForwardCallerAuth(t *testing.T) {
	h := Handler()
	body := []byte("no auth please")
	cid := cidForBody(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodPost, "/v1/retrievals/"+cid, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRetrievalEmptyObject(t *testing.T) {
	h := Handler()
	cid := cidForBody(nil)
	srv := octetServer(t, nil)
	defer srv.Close()
	publishRetrievalProvider(t, h, cid, "node-a", []string{srv.URL}, time.Now().Add(time.Hour))

	rec := retrievalRequest(t, h, cid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeRetrieval(t, rec)
	if resp.Size != 0 || len(resp.Chunks) != 0 {
		t.Fatalf("empty object response wrong: %+v", resp)
	}
	st := statsSnapshot(t, h)
	if st.objects != 1 || st.blocks != 0 || st.logicalBytes != 0 || st.storedBytes != 0 {
		t.Fatalf("empty object stats wrong: %+v", st)
	}
}
