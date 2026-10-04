package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// doReplication issues POST /v1/replications/{cid} with the given raw body.
func doReplication(t *testing.T, h http.Handler, cid string, rawBody string, contentType string, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/replications/"+cid, strings.NewReader(rawBody))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer caller-secret")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func replicationBody(targets []string, required int) string {
	raw, _ := json.Marshal(struct {
		Targets  []string `json:"targets"`
		Required int      `json:"required"`
	}{Targets: targets, Required: required})
	return string(raw)
}

func decodeReplication(t *testing.T, rec *httptest.ResponseRecorder) replicationResponse {
	t.Helper()
	var resp replicationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("replication response is not JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

// bundleTarget stands up a fresh CidVault instance behind an HTTP server and
// returns its bundle import URL.
func bundleTarget(t *testing.T) (string, http.Handler) {
	t.Helper()
	h := Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/bundles", h
}

func TestReplicationMethodMediaTypeAndCID(t *testing.T) {
	h := Handler()
	body := replicationBody([]string{"http://example.invalid/v1/bundles"}, 1)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/replications/"+testCID, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s Allow = %q, want POST", method, allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}

	// Invalid path CID wins over every body problem.
	rec := doReplication(t, h, "not-a-cid", "{garbage", "application/json", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CID status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_cid")

	// A valid CID with a wrong or missing media type is a media-type failure.
	rec = doReplication(t, h, testCID, body, "text/plain", false)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media type status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
	assertErrorCode(t, rec, "unsupported_media_type")
}

func TestReplicationObjectNotFoundAndValidationPrecedence(t *testing.T) {
	h := Handler()

	// A malformed body is rejected before the object is resolved.
	rec := doReplication(t, h, testCID, "{", "application/json", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_request")

	// A well-formed valid body reaches the object lookup, which fails.
	body := replicationBody([]string{"http://example.invalid/v1/bundles"}, 1)
	rec = doReplication(t, h, testCID, body, "application/json", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestReplicationRejectsSealedObjects(t *testing.T) {
	h := Handler()
	key := sealTestKey(7)
	rec := postSealed(t, h, []byte("sealed payload"), key, "application/octet-stream")
	sealed := decodeSealedResponse(t, rec)

	body := replicationBody([]string{"http://example.invalid/v1/bundles"}, 1)
	rec = doReplication(t, h, sealed.CID, body, "application/json", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sealed object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestReplicationDeliversAndClassifiesExisting(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("replicate me"))
	target, remote := bundleTarget(t)

	var mu sync.Mutex
	gotMethod, gotType, gotAuth, gotPath := "", "", "", ""
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		mu.Unlock()
		// The same bytes must be a valid full bundle for the source CID.
		var doc bundleDocument
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("target body read failed: %v", err)
		}
		if err := json.Unmarshal(rawBody, &doc); err != nil {
			t.Errorf("target body is not a bundle: %v", err)
		}
		if doc.Root.CID != up.CID || doc.Version != bundleVersion {
			t.Errorf("bundle root = %+v, want cid %s", doc.Root, up.CID)
		}
		// Restore the body before handing the request to the real instance.
		r.Body = io.NopCloser(bytes.NewReader(rawBody))
		r.ContentLength = int64(len(rawBody))
		remote.ServeHTTP(w, r)
	}))
	defer wrapped.Close()
	target = wrapped.URL + "/v1/bundles"

	rec := doReplication(t, h, up.CID, replicationBody([]string{target}, 1), "application/json", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeReplication(t, rec)
	if resp.CID != up.CID || resp.Required != 1 || resp.Succeeded != 1 || resp.Failed != 0 || !resp.MetRequirement {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(resp.Results) != 1 || !resp.Results[0].OK || resp.Results[0].Result != "created" ||
		resp.Results[0].Target != target {
		t.Fatalf("unexpected results: %+v", resp.Results)
	}
	mu.Lock()
	if gotMethod != http.MethodPost || gotPath != "/v1/bundles" || gotType != bundleMediaType {
		t.Fatalf("target saw method=%q path=%q type=%q", gotMethod, gotPath, gotType)
	}
	if gotAuth != "" {
		t.Fatalf("target received caller credentials %q", gotAuth)
	}
	mu.Unlock()

	// The remote imported the object and deduces the same CID.
	req := httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID, nil)
	rrec := httptest.NewRecorder()
	remote.ServeHTTP(rrec, req)
	if rrec.Code != http.StatusOK || rrec.Body.String() != "replicate me" {
		t.Fatalf("remote object status = %d body = %q", rrec.Code, rrec.Body.String())
	}

	// A second delivery hits the existing object and reports "existing".
	rec = doReplication(t, h, up.CID, replicationBody([]string{target}, 1), "application/json", false)
	resp = decodeReplication(t, rec)
	if resp.Succeeded != 1 || resp.Results[0].Result != "existing" {
		t.Fatalf("second replication = %+v, want existing", resp.Results)
	}
}

func TestReplicationParallelFanOut(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("fan out"))

	const n = 4
	var targets []string
	var mu sync.Mutex
	arrived := 0
	var release sync.WaitGroup
	release.Add(1)
	for range n {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			arrived++
			mu.Unlock()
			release.Wait() // all deliveries must be in flight before any answers
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"cid": up.CID, "created": true})
		}))
		t.Cleanup(srv.Close)
		targets = append(targets, srv.URL+"/v1/bundles")
	}
	sort.Strings(targets)

	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- doReplication(t, h, up.CID, replicationBody(targets, n), "application/json", false)
	}()
	time.Sleep(200 * time.Millisecond) // let every delivery arrive
	mu.Lock()
	gotArrived := arrived
	mu.Unlock()
	release.Done()
	rec := <-finished
	if gotArrived != n {
		t.Fatalf("only %d of %d deliveries were in flight concurrently", gotArrived, n)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReplicationFailureClassificationAndOrder(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("classify me"))

	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close() // its address now refuses connections
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()
	otherCID := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"cid":%q,"created":true}`, testCID)
	}))
	defer otherCID.Close()
	notBool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"cid":%q,"created":"yes"}`, up.CID)
	}))
	defer notBool.Close()
	noCreated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"cid":%q}`, up.CID)
	}))
	defer noCreated.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverErr.Close()
	good, _ := bundleTarget(t)

	targets := []string{
		closed.URL + "/v1/bundles",
		redirect.URL + "/v1/bundles",
		otherCID.URL + "/v1/bundles",
		notBool.URL + "/v1/bundles",
		noCreated.URL + "/v1/bundles",
		garbage.URL + "/v1/bundles",
		serverErr.URL + "/v1/bundles",
		good,
	}
	sort.Strings(targets)
	want := map[string]string{
		closed.URL + "/v1/bundles":    "unreachable",
		redirect.URL + "/v1/bundles":  "rejected",
		otherCID.URL + "/v1/bundles":  "rejected",
		notBool.URL + "/v1/bundles":   "rejected",
		noCreated.URL + "/v1/bundles": "rejected",
		garbage.URL + "/v1/bundles":   "rejected",
		serverErr.URL + "/v1/bundles": "rejected",
		good:                          "created",
	}

	// One success cannot meet a requirement of eight, but every target is
	// still attempted.
	rec := doReplication(t, h, up.CID, replicationBody(targets, len(targets)), "application/json", false)
	resp := decodeReplication(t, rec)
	if resp.Succeeded != 1 || resp.Failed != len(targets)-1 || resp.MetRequirement {
		t.Fatalf("counts = +%d/-%d met=%v, want 1/%d/false", resp.Succeeded, resp.Failed, resp.MetRequirement, len(targets)-1)
	}
	if len(resp.Results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(resp.Results), len(targets))
	}
	for i, res := range resp.Results {
		if res.Target != targets[i] {
			t.Fatalf("result %d target = %q, want %q", i, res.Target, targets[i])
		}
		if res.Result != want[res.Target] || res.OK != (res.Result == "created" || res.Result == "existing") {
			t.Fatalf("result for %s = %+v, want %s", res.Target, res, want[res.Target])
		}
	}

	// Requirement one is met despite the failures; nothing is rolled back.
	rec = doReplication(t, h, up.CID, replicationBody(targets, 1), "application/json", false)
	resp = decodeReplication(t, rec)
	if !resp.MetRequirement || resp.Succeeded != 1 {
		t.Fatalf("metRequirement = %v with %d successes, want true/1", resp.MetRequirement, resp.Succeeded)
	}
}

func TestReplicationDeliveryTimeoutIsUnreachable(t *testing.T) {
	if got, want := replicationTimeout, 5*time.Second; got != want {
		t.Fatalf("replicationTimeout = %v, want %v", got, want)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	rp := newReplicator(newStore())
	rp.client = &http.Client{
		Timeout: 50 * time.Millisecond,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ok, result := rp.deliver(srv.URL+"/v1/bundles", []byte("{}"), testCID)
	if ok || result != "unreachable" {
		t.Fatalf("timeout delivery = %v/%q, want false/unreachable", ok, result)
	}
}

func TestReplicationValidationFailuresNeverContactNetwork(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("no touch"))

	var hit bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
	}))
	defer srv.Close()
	safe := srv.URL + "/v1/bundles"

	cases := []struct {
		name       string
		body       string
		mediaType  string
		wantStatus int
		wantCode   string
	}{
		{"not json", `{"targets":`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"trailing content", `{"targets":["` + safe + `"],"required":1} {}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"duplicate key", `{"targets":["` + safe + `"],"targets":["` + safe + `"],"required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"unknown field", `{"targets":["` + safe + `"],"required":1,"extra":2}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"missing required", `{"targets":["` + safe + `"]}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"missing targets", `{"required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"null targets", `{"targets":null,"required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"null required", `{"targets":["` + safe + `"],"required":null}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"targets not array", `{"targets":"` + safe + `","required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"target element null", `{"targets":[null],"required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"target element number", `{"targets":[1],"required":1}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"required string", `{"targets":["` + safe + `"],"required":"1"}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"required fraction", `{"targets":["` + safe + `"],"required":1.5}`, "application/json", http.StatusBadRequest, "invalid_request"},
		{"empty targets", `{"targets":[],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"required zero", `{"targets":["` + safe + `"],"required":0}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"required negative", `{"targets":["` + safe + `"],"required":-1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"required too large", `{"targets":["` + safe + `"],"required":2}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"required overflow", `{"targets":["` + safe + `"],"required":99999999999999999999}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"duplicate targets", `{"targets":["` + safe + `","` + safe + `"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"unsorted targets", `{"targets":["` + safe + "/x" + `","` + safe + `"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"query", `{"targets":["` + safe + `?x=1"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"fragment", `{"targets":["` + safe + `#f"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"userinfo", `{"targets":["http://u:p@` + strings.TrimPrefix(srv.URL, "http://") + `/v1/bundles"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"trailing slash", `{"targets":["` + safe + `/"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"wrong path", `{"targets":["` + srv.URL + `/v1/objects"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"encoded path", `{"targets":["` + srv.URL + `/v1/bundle%73"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"wrong scheme", `{"targets":["ftp://example.invalid/v1/bundles"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"relative url", `{"targets":["/v1/bundles"],"required":1}`, "application/json", http.StatusUnprocessableEntity, "invalid_replication_request"},
		{"wrong media type", `{"targets":["` + safe + `"],"required":1}`, "text/plain", http.StatusUnsupportedMediaType, "unsupported_media_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReplication(t, h, up.CID, tc.body, tc.mediaType, false)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertErrorCode(t, rec, tc.wantCode)
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if hit {
		t.Fatal("a validation failure contacted a target")
	}
}

func TestReplicationSeventeenTargetsRejected(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("too many"))
	var elems []string
	for i := range 17 {
		elems = append(elems, fmt.Sprintf("http://node%02d.invalid/v1/bundles", i))
	}
	sort.Strings(elems)
	rec := doReplication(t, h, up.CID, replicationBody(elems, 1), "application/json", false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_replication_request")
}

func TestReplicationLeavesLocalStateAndAuditUntouched(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("local stays"))
	target, _ := bundleTarget(t)

	snapshot := func() (statsResponse, auditResponse) {
		req := httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var st statsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(http.MethodGet, "/v1/audit/events?limit=1000", nil)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var ev auditResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		return st, ev
	}
	statsBefore, auditBefore := snapshot()

	for range 2 {
		rec := doReplication(t, h, up.CID, replicationBody([]string{target}, 1), "application/json", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}

	statsAfter, auditAfter := snapshot()
	if statsAfter != statsBefore {
		t.Fatalf("local stats changed: before %+v after %+v", statsBefore, statsAfter)
	}
	// The upload audited itself; the two replications append nothing.
	if auditBefore.NextAfter != auditAfter.NextAfter || len(auditBefore.Events) != len(auditAfter.Events) {
		t.Fatalf("local audit changed: before %+v after %+v", auditBefore, auditAfter)
	}
	for _, ev := range auditAfter.Events {
		if ev.Action == "bundle.import" {
			t.Fatalf("local audit unexpectedly contains a bundle import: %+v", ev)
		}
	}
}

func TestReplicationResultsFollowTargetOrder(t *testing.T) {
	h := Handler()
	payload := []byte("ordering")
	up := uploadObject(t, h, payload)
	doc, _ := buildBundle(t, payload)

	target0, remote0 := bundleTarget(t)
	target1, remote1 := bundleTarget(t)
	targets := []string{target0, target1}
	sort.Strings(targets)
	// Preload the object on the lexicographically first target so it answers
	// existing; the other must answer created.
	preload := remote0
	if targets[0] != target0 {
		preload = remote1
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if rec := postBundleBody(t, preload, raw, bundleMediaType); rec.Code != http.StatusCreated {
		t.Fatalf("preload status = %d: %s", rec.Code, rec.Body.String())
	}

	rec := doReplication(t, h, up.CID, replicationBody(targets, 1), "application/json", false)
	resp := decodeReplication(t, rec)
	want := map[string]string{targets[0]: "existing", targets[1]: "created"}
	for i, res := range resp.Results {
		if res.Target != targets[i] || !res.OK || res.Result != want[targets[i]] {
			t.Fatalf("result %d = %+v, want target %s result %s", i, res, targets[i], want[targets[i]])
		}
	}
}
