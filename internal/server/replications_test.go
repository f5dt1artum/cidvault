package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func doReplication(t *testing.T, h http.Handler, method, cid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/replications/"+cid, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeReplication(t *testing.T, rec *httptest.ResponseRecorder) replicationResponse {
	t.Helper()
	var resp replicationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("replication response is not JSON: %v", err)
	}
	return resp
}

// replicationBody renders a request body for the given targets, sorting them
// into the required strictly ascending order first.
func replicationBody(targets []string, required int) string {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, target := range sorted {
		quoted[i] = fmt.Sprintf("%q", target)
	}
	return fmt.Sprintf(`{"targets":[%s],"required":%d}`, strings.Join(quoted, ","), required)
}

func TestReplicationMethodAndCIDValidation(t *testing.T) {
	h := Handler()

	rec := doReplication(t, h, http.MethodPost, "not-a-cid", `{"targets":[],"required":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid CID status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_cid")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doReplication(t, h, method, testCID, `{}`)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s Allow = %q, want %q", method, allow, http.MethodPost)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestReplicationObjectNotFound(t *testing.T) {
	h := Handler()
	rec := doReplication(t, h, http.MethodPost, testCID,
		replicationBody([]string{"http://127.0.0.1:1/v1/bundles"}, 1))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestReplicationSealedObjectNotReplicated(t *testing.T) {
	h := Handler()
	rec := postSealed(t, h, []byte("envelope"), sealTestKey(1), "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seal status = %d, want %d", rec.Code, http.StatusCreated)
	}
	cid := decodeSealedResponse(t, rec).CID
	rec = doReplication(t, h, http.MethodPost, cid,
		replicationBody([]string{"http://127.0.0.1:1/v1/bundles"}, 1))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestReplicationMediaType(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("media type check"))

	for _, ct := range []string{"", "text/plain", "application/vnd.cidvault.bundle+json"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/replications/"+up.CID,
			strings.NewReader(replicationBody([]string{"http://127.0.0.1:1/v1/bundles"}, 1)))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}
}

func TestReplicationMalformedRequests(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("strict body"))
	target := "http://127.0.0.1:1/v1/bundles"

	bodies := map[string]string{
		"not JSON":          `{`,
		"trailing content":  `{"targets":[],"required":1} {}`,
		"empty object":      `{}`,
		"missing targets":   `{"required":1}`,
		"missing required":  `{"targets":["` + target + `"]}`,
		"unknown field":     `{"targets":["` + target + `"],"required":1,"extra":true}`,
		"duplicate field":   `{"targets":["` + target + `"],"targets":["` + target + `"],"required":1}`,
		"targets null":      `{"targets":null,"required":1}`,
		"targets string":    `{"targets":"` + target + `","required":1}`,
		"target non-string": `{"targets":[1],"required":1}`,
		"target null":       `{"targets":[null],"required":1}`,
		"required null":     `{"targets":["` + target + `"],"required":null}`,
		"required string":   `{"targets":["` + target + `"],"required":"1"}`,
		"required fraction": `{"targets":["` + target + `"],"required":1.5}`,
		"required bool":     `{"targets":["` + target + `"],"required":true}`,
	}
	for name, body := range bodies {
		rec := doReplication(t, h, http.MethodPost, up.CID, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d", name, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestReplicationInvalidValues(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("value validation"))

	// A valid target that must never be contacted by these failing requests.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("target contacted although the request is invalid")
	}))
	defer srv.Close()
	valid := srv.URL + "/v1/bundles"

	bodies := map[string]string{
		"targets empty":       `{"targets":[],"required":1}`,
		"targets duplicate":   `{"targets":["` + valid + `","` + valid + `"],"required":1}`,
		"targets unsorted":    `{"targets":["` + valid + `","http://127.0.0.1:1/v1/bundles"],"required":1}`,
		"target not absolute": `{"targets":["/v1/bundles"],"required":1}`,
		"target bad scheme":   `{"targets":["ftp://example.com/v1/bundles"],"required":1}`,
		"target userinfo":     `{"targets":["http://user@example.com/v1/bundles"],"required":1}`,
		"target empty host":   `{"targets":["http:///v1/bundles"],"required":1}`,
		"target wrong path":   `{"targets":["http://example.com/v1/objects"],"required":1}`,
		"target no path":      `{"targets":["http://example.com"],"required":1}`,
		"target longer path":  `{"targets":["http://example.com/v1/bundles/x"],"required":1}`,
		"target query":        `{"targets":["http://example.com/v1/bundles?x=1"],"required":1}`,
		"target empty query":  `{"targets":["http://example.com/v1/bundles?"],"required":1}`,
		"target fragment":     `{"targets":["http://example.com/v1/bundles#f"],"required":1}`,
		"target empty frag":   `{"targets":["http://example.com/v1/bundles#"],"required":1}`,
		"required zero":       `{"targets":["` + valid + `"],"required":0}`,
		"required negative":   `{"targets":["` + valid + `"],"required":-1}`,
		"required too large":  `{"targets":["` + valid + `"],"required":2}`,
		"required huge":       `{"targets":["` + valid + `"],"required":99999999999999999999999}`,
	}
	for name, body := range bodies {
		rec := doReplication(t, h, http.MethodPost, up.CID, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want %d", name, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_replication_request")
	}

	// Seventeen targets exceed the bound even when all are valid.
	targets := make([]string, 17)
	for i := range targets {
		targets[i] = fmt.Sprintf("http://example.com:%d/v1/bundles", i)
	}
	rec := doReplication(t, h, http.MethodPost, up.CID, replicationBody(targets, 1))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("17 targets: status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
	assertErrorCode(t, rec, "invalid_replication_request")
}

// remoteVault wraps a full peer instance, recording the headers each
// delivered bundle arrives with.
type remoteVault struct {
	srv  *httptest.Server
	peer http.Handler
	mu   sync.Mutex
	// headers of the received import requests, in arrival order
	contentTypes []string
	auths        []string
}

func newRemoteVault(t *testing.T) *remoteVault {
	t.Helper()
	rv := &remoteVault{}
	rv.peer = Handler()
	rv.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rv.mu.Lock()
		rv.contentTypes = append(rv.contentTypes, r.Header.Get("Content-Type"))
		rv.auths = append(rv.auths, r.Header.Get("Authorization"))
		rv.mu.Unlock()
		rv.peer.ServeHTTP(w, r)
	}))
	t.Cleanup(rv.srv.Close)
	return rv
}

func (rv *remoteVault) target() string { return rv.srv.URL + "/v1/bundles" }

func (rv *remoteVault) received(t *testing.T, want int) {
	t.Helper()
	rv.mu.Lock()
	defer rv.mu.Unlock()
	if len(rv.contentTypes) != want {
		t.Fatalf("remote received %d requests, want %d", len(rv.contentTypes), want)
	}
	for i, ct := range rv.contentTypes {
		if ct != bundleMediaType {
			t.Fatalf("request %d Content-Type = %q, want %q", i, ct, bundleMediaType)
		}
		if rv.auths[i] != "" {
			t.Fatalf("request %d carried Authorization %q, want none", i, rv.auths[i])
		}
	}
}

func TestReplicationDeliversBundle(t *testing.T) {
	h := Handler()
	body := []byte(strings.Repeat("replicate-", 200000)) // spans two chunks
	up := uploadObject(t, h, body)

	fresh := newRemoteVault(t)
	loaded := newRemoteVault(t)
	// The second peer already holds the object, so its import deduplicates.
	uploadObject(t, loaded.peer, body)

	targets := []string{fresh.target(), loaded.target()}
	sort.Strings(targets)
	req := httptest.NewRequest(http.MethodPost, "/v1/replications/"+up.CID,
		strings.NewReader(replicationBody(targets, 2)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer must-not-leak")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeReplication(t, rec)
	if resp.CID != up.CID || resp.Required != 2 {
		t.Fatalf("response cid/required = %q/%d, want %q/2", resp.CID, resp.Required, up.CID)
	}
	if resp.Succeeded != 2 || resp.Failed != 0 || !resp.MetRequirement {
		t.Fatalf("succeeded/failed/met = %d/%d/%v, want 2/0/true",
			resp.Succeeded, resp.Failed, resp.MetRequirement)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(resp.Results))
	}
	wantResult := map[string]string{fresh.target(): "created", loaded.target(): "existing"}
	for i, res := range resp.Results {
		if res.Target != targets[i] {
			t.Fatalf("results[%d].target = %q, want %q (target order)", i, res.Target, targets[i])
		}
		if !res.OK || res.Result != wantResult[res.Target] {
			t.Fatalf("results[%d] = %+v, want ok with %q", i, res, wantResult[res.Target])
		}
	}

	// Both peers received exactly one bundle each, with the bundle media
	// type and no forwarded credentials.
	fresh.received(t, 1)
	loaded.received(t, 1)

	// The fresh peer now serves the object byte for byte.
	req = httptest.NewRequest(http.MethodGet, "/v1/objects/"+up.CID, nil)
	rec = httptest.NewRecorder()
	fresh.peer.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("peer GET object status = %d, body mismatch", rec.Code)
	}

	// Replication changed no local state and wrote no local audit events:
	// the only local event remains the initial upload.
	req = httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var audit auditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("audit response is not JSON: %v", err)
	}
	if len(audit.Events) != 1 || audit.Events[0].Action != auditObjectUpload {
		t.Fatalf("local audit events = %+v, want only the upload", audit.Events)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("stats response is not JSON: %v", err)
	}
	if st.Objects != 1 || st.LogicalBytes != len(body) {
		t.Fatalf("stats = %+v, want the single uploaded object", st)
	}
}

func TestReplicationMixedOutcomes(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("mixed delivery"))

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer redirectSrv.Close()
	errorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errorSrv.Close()
	nonJSONSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer nonJSONSrv.Close()
	wrongCIDSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cid":"` + testCID + `","created":true}`))
	}))
	defer wrongCIDSrv.Close()
	wrongTypeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cid":"` + up.CID + `","created":"yes"}`))
	}))
	defer wrongTypeSrv.Close()
	ok := newRemoteVault(t)

	unreachable := "http://127.0.0.1:1/v1/bundles"
	targets := []string{
		unreachable,
		redirectSrv.URL + "/v1/bundles",
		errorSrv.URL + "/v1/bundles",
		nonJSONSrv.URL + "/v1/bundles",
		wrongCIDSrv.URL + "/v1/bundles",
		wrongTypeSrv.URL + "/v1/bundles",
		ok.target(),
	}
	sort.Strings(targets)
	rec := doReplication(t, h, http.MethodPost, up.CID, replicationBody(targets, 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeReplication(t, rec)
	if resp.Succeeded != 1 || resp.Failed != 6 || resp.MetRequirement {
		t.Fatalf("succeeded/failed/met = %d/%d/%v, want 1/6/false",
			resp.Succeeded, resp.Failed, resp.MetRequirement)
	}
	want := map[string]replicationResult{
		unreachable:                      {OK: false, Result: "unreachable"},
		redirectSrv.URL + "/v1/bundles":  {OK: false, Result: "rejected"},
		errorSrv.URL + "/v1/bundles":     {OK: false, Result: "rejected"},
		nonJSONSrv.URL + "/v1/bundles":   {OK: false, Result: "rejected"},
		wrongCIDSrv.URL + "/v1/bundles":  {OK: false, Result: "rejected"},
		wrongTypeSrv.URL + "/v1/bundles": {OK: false, Result: "rejected"},
		ok.target():                      {OK: true, Result: "created"},
	}
	if len(resp.Results) != len(targets) {
		t.Fatalf("results = %d, want %d", len(resp.Results), len(targets))
	}
	for i, res := range resp.Results {
		if res.Target != targets[i] {
			t.Fatalf("results[%d].target = %q, want %q", i, res.Target, targets[i])
		}
		w := want[res.Target]
		if res.OK != w.OK || res.Result != w.Result {
			t.Fatalf("results[%d] = %+v, want ok=%v result=%q", i, res, w.OK, w.Result)
		}
	}
	ok.received(t, 1)
}

func TestReplicationTimeoutIsUnreachable(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, []byte("slow target"))

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(replicationTimeout + 2*time.Second)
	}))
	defer slow.Close()
	ok := newRemoteVault(t)

	targets := []string{slow.URL + "/v1/bundles", ok.target()}
	sort.Strings(targets)
	start := time.Now()
	rec := doReplication(t, h, http.MethodPost, up.CID, replicationBody(targets, 1))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeReplication(t, rec)
	if resp.Succeeded != 1 || resp.Failed != 1 || !resp.MetRequirement {
		t.Fatalf("succeeded/failed/met = %d/%d/%v, want 1/1/true",
			resp.Succeeded, resp.Failed, resp.MetRequirement)
	}
	for _, res := range resp.Results {
		switch res.Target {
		case slow.URL + "/v1/bundles":
			if res.OK || res.Result != "unreachable" {
				t.Fatalf("slow target = %+v, want unreachable", res)
			}
		case ok.target():
			if !res.OK || res.Result != "created" {
				t.Fatalf("peer target = %+v, want created", res)
			}
		}
	}
	if elapsed > replicationTimeout+time.Second {
		t.Fatalf("replication took %v, want about the %v per-target timeout", elapsed, replicationTimeout)
	}
}

func TestReplicationEmptyObject(t *testing.T) {
	h := Handler()
	up := uploadObject(t, h, nil)
	peer := newRemoteVault(t)

	rec := doReplication(t, h, http.MethodPost, up.CID, replicationBody([]string{peer.target()}, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeReplication(t, rec)
	if resp.Succeeded != 1 || !resp.MetRequirement || !resp.Results[0].OK ||
		resp.Results[0].Result != "created" {
		t.Fatalf("response = %+v, want one created delivery", resp)
	}
}
