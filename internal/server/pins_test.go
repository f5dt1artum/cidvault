package server

import (
	"bytes"
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

func uploadObject(t *testing.T, h http.Handler, body []byte) objectResponse {
	t.Helper()
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return decodeObjectResponse(t, rec)
}

func putPinRaw(t *testing.T, h http.Handler, cid string, body string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/pins/"+cid, rdr)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodePin(t *testing.T, rec *httptest.ResponseRecorder) pinResponse {
	t.Helper()
	var resp pinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("pin response is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

func listPins(t *testing.T, h http.Handler) []pinResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list pins status = %d", rec.Code)
	}
	var resp pinListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list pins is not JSON: %v", err)
	}
	return resp.Pins
}

func runGC(t *testing.T, h http.Handler, dryRun string) gcResponse {
	t.Helper()
	url := "/v1/gc"
	if dryRun != "" {
		url += "?dryRun=" + dryRun
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("gc status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp gcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("gc response is not JSON: %v", err)
	}
	return resp
}

func TestPinCreateUpdateDeleteLifecycle(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("pinned content"))

	// First pin: 201, permanent.
	rec := putPinRaw(t, h, obj.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pin status = %d, want %d (body=%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	pin := decodePin(t, rec)
	if pin.CID != obj.CID || pin.ExpiresAt != nil {
		t.Fatalf("unexpected create response: %+v", pin)
	}

	// Second effective pin: 200.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = putPinRaw(t, h, obj.CID, fmt.Sprintf(`{"expiresAt":%q}`, future), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update pin status = %d, want %d", rec.Code, http.StatusOK)
	}
	pin = decodePin(t, rec)
	if pin.CID != obj.CID || pin.ExpiresAt == nil {
		t.Fatalf("unexpected update response: %+v", pin)
	}

	// Back to permanent: still 200.
	rec = putPinRaw(t, h, obj.CID, `{"expiresAt":null}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset pin status = %d, want %d", rec.Code, http.StatusOK)
	}
	if decodePin(t, rec).ExpiresAt != nil {
		t.Fatal("expiresAt should be null for permanent pin")
	}

	pins := listPins(t, h)
	if len(pins) != 1 || pins[0].CID != obj.CID || pins[0].ExpiresAt != nil {
		t.Fatalf("unexpected pin list: %+v", pins)
	}

	// Delete: 204, then 404.
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil))
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete pin status = %d, want %d", delRec.Code, http.StatusNoContent)
	}
	if delRec.Body.Len() != 0 {
		t.Fatalf("204 response must have no body, got %q", delRec.Body.String())
	}
	delRec = httptest.NewRecorder()
	h.ServeHTTP(delRec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil))
	if delRec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", delRec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, delRec, "pin_not_found")

	if pins := listPins(t, h); len(pins) != 0 {
		t.Fatalf("pin list should be empty, got %+v", pins)
	}

	// Deleting the pin does not delete the object.
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("object should survive pin removal, got %d", getRec.Code)
	}
}

func TestPinMissingObjectAndBadCID(t *testing.T) {
	h := Handler()
	missing := "sha256:" + strings.Repeat("0", 64)

	rec := putPinRaw(t, h, missing, `{}`, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pin missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	for _, bad := range []string{"not-a-cid", "sha256:ABC", "sha256:" + strings.Repeat("g", 64)} {
		rec := putPinRaw(t, h, bad, `{}`, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("pin bad cid %q status = %d, want %d", bad, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")

		delRec := httptest.NewRecorder()
		h.ServeHTTP(delRec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+bad, nil))
		if delRec.Code != http.StatusBadRequest {
			t.Fatalf("delete bad cid %q status = %d, want %d", bad, delRec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, delRec, "invalid_cid")
	}

	// Valid shape but no pin exists: pin_not_found, not object_not_found.
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+missing, nil))
	if delRec.Code != http.StatusNotFound {
		t.Fatalf("delete missing pin status = %d, want %d", delRec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, delRec, "pin_not_found")
}

func TestPinMediaTypeRequired(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("media"))
	for _, ct := range []string{"text/plain", ""} {
		rec := putPinRaw(t, h, obj.CID, `{}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}
	// charset parameter on the JSON media type is still application/json.
	rec := putPinRaw(t, h, obj.CID, `{}`, "application/json; charset=utf-8")
	if rec.Code != http.StatusCreated {
		t.Fatalf("application/json with charset status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestPinInvalidRequestBodies(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("bodies"))
	bodies := []string{
		"",
		"{",
		"not-json",
		"123",
		"true",
		"[]",
		"null",
		`{"unknown":1}`,
		`{"expiresAt":null,"extra":0}`,
		`{}{}`,
	}
	for _, body := range bodies {
		rec := putPinRaw(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestPinInvalidExpiration(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("expiry"))
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	bodies := []string{
		`{"expiresAt":123}`,
		`{"expiresAt":false}`,
		`{"expiresAt":{}}`,
		`{"expiresAt":"not-a-time"}`,
		fmt.Sprintf(`{"expiresAt":%q}`, past),
		// 2020 is strictly in the past regardless of offset.
		`{"expiresAt":"2020-01-01T00:00:00Z"}`,
		`{"expiresAt":"2020-01-01T02:00:00+02:00"}`,
	}
	for _, body := range bodies {
		rec := putPinRaw(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_expiration")
	}

	// A strictly future timestamp with a non-UTC offset is accepted.
	futureOffset := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05-07:00")
	rec := putPinRaw(t, h, obj.CID, fmt.Sprintf(`{"expiresAt":%q}`, futureOffset), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("future offset timestamp status = %d, want %d (body=%s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestPinListSortedAndExcludesExpired(t *testing.T) {
	h := Handler()
	cids := []string{
		uploadObject(t, h, []byte("ccc")).CID,
		uploadObject(t, h, []byte("aaa")).CID,
		uploadObject(t, h, []byte("bbb")).CID,
	}
	if cids[0] == cids[1] || cids[1] == cids[2] || cids[0] == cids[2] {
		t.Fatal("test payloads must produce distinct cids")
	}

	// Pin in deliberately unsorted order.
	for i, cid := range []string{cids[2], cids[0], cids[1]} {
		var body string
		if i == 1 {
			body = fmt.Sprintf(`{"expiresAt":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		} else {
			body = `{}`
		}
		if rec := putPinRaw(t, h, cid, body, "application/json"); rec.Code != http.StatusCreated {
			t.Fatalf("pin setup status = %d", rec.Code)
		}
	}

	pins := listPins(t, h)
	if len(pins) != 3 {
		t.Fatalf("pins = %d, want 3", len(pins))
	}
	got := []string{pins[0].CID, pins[1].CID, pins[2].CID}
	want := append([]string(nil), got...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pins not sorted by cid: %v", got)
	}

	// Empty store reports an explicit empty array.
	emptyH := Handler()
	emptyRec := httptest.NewRecorder()
	emptyH.ServeHTTP(emptyRec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if !strings.Contains(emptyRec.Body.String(), `"pins":[]`) {
		t.Fatalf("empty pin list should encode as [], got %s", emptyRec.Body.String())
	}
}

func TestExpiredPinSemanticsAtStoreLevel(t *testing.T) {
	s := newStore()
	cid := "sha256:" + strings.Repeat("a", 64)
	s.objects[cid] = &object{cid: cid, size: 5, chunks: [][]byte{[]byte("hello")}, cids: []string{chunkCID([]byte("hello"))}}

	t0 := time.Unix(1000, 0)
	expiry := t0.Add(time.Minute)

	if updated, ok := s.upsertPin(cid, &expiry, t0); !ok || updated {
		t.Fatalf("upsertPin = (%v, %v), want (false, true)", updated, ok)
	}
	pins := s.listPins(t0)
	if len(pins) != 1 || pins[0].expiresAt == nil {
		t.Fatalf("valid pin missing at t0: %+v", pins)
	}
	if s.deletePin(cid, t0.Add(2*time.Minute)) {
		t.Fatal("deleting an expired pin should fail")
	}
	if pins := s.listPins(t0.Add(2 * time.Minute)); len(pins) != 0 {
		t.Fatalf("expired pin should not be listed: %+v", pins)
	}

	// Expired pin makes the object a GC candidate; dry run changes nothing.
	res := s.collectGarbage(t0.Add(2*time.Minute), true)
	if len(res.cids) != 1 || res.cids[0] != cid || res.bytes != 5 {
		t.Fatalf("dry run candidate = %+v, want only expired object, bytes 5", res)
	}
	if _, exists := s.objects[cid]; !exists {
		t.Fatal("dry run must not delete objects")
	}

	// A second dry run is still just a preview.
	if res := s.collectGarbage(t0.Add(2*time.Minute), true); len(res.cids) != 1 {
		t.Fatalf("second dry run candidates = %v, want 1", res.cids)
	}

	// Actually collect: the object and its expired pin record are removed.
	res = s.collectGarbage(t0.Add(2*time.Minute), false)
	if len(res.cids) != 1 || res.bytes != 5 {
		t.Fatalf("collection = %+v, want the expired object", res)
	}
	if _, exists := s.objects[cid]; exists {
		t.Fatal("object should be gone after collection")
	}
	if _, pinned := s.pins[cid]; pinned {
		t.Fatal("expired pin record should be removed with its object")
	}

	// Once deleted, pinning fails with object-not-found.
	if _, ok := s.upsertPin(cid, nil, t0.Add(2*time.Minute)); ok {
		t.Fatal("pinning a collected object must report it missing")
	}

	// Reinsert with a permanent pin: it is protected from collection.
	s.objects[cid] = &object{cid: cid, size: 5}
	if updated, ok := s.upsertPin(cid, nil, t0.Add(3*time.Minute)); !ok || updated {
		t.Fatalf("permanent upsertPin = (%v, %v), want (false, true)", updated, ok)
	}
	if res := s.collectGarbage(t0.Add(3*time.Minute), false); len(res.cids) != 0 {
		t.Fatalf("permanently pinned object must be kept, candidates = %v", res.cids)
	}
}

func TestGarbageCollectionEndToEnd(t *testing.T) {
	h := Handler()
	keep := uploadObject(t, h, []byte("keep me forever")) // 15 bytes, pinned permanent
	drop1 := uploadObject(t, h, []byte("drop-one"))       // 9 bytes, unpinned
	drop2 := uploadObject(t, h, []byte("dd"))             // 2 bytes, pin expires

	if rec := putPinRaw(t, h, keep.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin keep status = %d", rec.Code)
	}
	if rec := putPinRaw(t, h, drop2.CID,
		fmt.Sprintf(`{"expiresAt":%q}`, time.Now().Add(400*time.Millisecond).UTC().Format(time.RFC3339Nano)),
		"application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin drop2 status = %d", rec.Code)
	}

	// Before expiry, GC has exactly one candidate and nothing is deleted.
	pre := runGC(t, h, "true")
	if !pre.DryRun || pre.Objects != 1 || pre.Bytes != len("drop-one") {
		t.Fatalf("pre-expiry dry run = %+v, want single unpinned candidate", pre)
	}
	if pre.CIDs[0] != drop1.CID {
		t.Fatalf("candidate cid = %q, want %q", pre.CIDs[0], drop1.CID)
	}
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+drop1.CID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("dry run must not delete, got status %d", getRec.Code)
	}

	// Wait for the second pin to expire.
	time.Sleep(450 * time.Millisecond)

	preview := runGC(t, h, "true")
	if !preview.DryRun || preview.Objects != 2 || preview.Bytes != len("drop-one")+len("dd") {
		t.Fatalf("post-expiry preview = %+v, want 2 candidates / %d bytes", preview, len("drop-one")+len("dd"))
	}
	wantCIDs := []string{drop1.CID, drop2.CID}
	sort.Strings(wantCIDs)
	if strings.Join(preview.CIDs, ",") != strings.Join(wantCIDs, ",") {
		t.Fatalf("candidate cids = %v, want sorted %v", preview.CIDs, wantCIDs)
	}

	// Actual collection.
	done := runGC(t, h, "")
	if done.DryRun || done.Objects != 2 || done.Bytes != preview.Bytes {
		t.Fatalf("gc result = %+v, want 2 deleted objects", done)
	}

	for _, cid := range wantCIDs {
		for _, suffix := range []string{"", "/manifest"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid+suffix, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s%s after gc status = %d, want 404", cid, suffix, rec.Code)
			}
			assertErrorCode(t, rec, "object_not_found")
		}
	}

	// Pinned object survives; expired pin record is gone with its object.
	getRec = httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+keep.CID, nil))
	if getRec.Code != http.StatusOK || !bytes.Equal(getRec.Body.Bytes(), []byte("keep me forever")) {
		t.Fatalf("pinned object damaged: status %d", getRec.Code)
	}
	pins := listPins(t, h)
	if len(pins) != 1 || pins[0].CID != keep.CID {
		t.Fatalf("pin list after gc = %+v, want only keep", pins)
	}
	// Re-pinning the deleted object fails.
	if rec := putPinRaw(t, h, drop1.CID, `{}`, "application/json"); rec.Code != http.StatusNotFound {
		t.Fatalf("repin deleted object status = %d, want 404", rec.Code)
	}

	// Re-upload identical content: original cid, reported as newly created.
	reup := uploadObject(t, h, []byte("drop-one"))
	if reup.CID != drop1.CID || !reup.Created {
		t.Fatalf("re-upload = %+v, want original cid %s created=true", reup, drop1.CID)
	}

	// The re-uploaded object is unpinned, so the next GC removes it again.
	again := runGC(t, h, "")
	if again.Objects != 1 || again.Bytes != len("drop-one") || len(again.CIDs) != 1 || again.CIDs[0] != drop1.CID {
		t.Fatalf("re-uploaded unpinned object gc = %+v, want %s", again, drop1.CID)
	}

	// With everything pinned, a further GC reports an empty list.
	after := runGC(t, h, "")
	if after.Objects != 0 || after.Bytes != 0 || len(after.CIDs) != 0 {
		t.Fatalf("follow-up gc = %+v, want empty result", after)
	}
}

func TestGCDryRunInvalidValue(t *testing.T) {
	h := Handler()
	for _, v := range []string{"false", "1", "yes", "TRUE", "null"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/gc?dryRun="+v, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("dryRun=%q status = %d, want 400", v, rec.Code)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestPinsGCMethodNotAllowed(t *testing.T) {
	h := Handler()
	cid := strings.Repeat("0", 64)
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodPost, "/v1/pins", http.MethodGet},
		{http.MethodPut, "/v1/pins", http.MethodGet},
		{http.MethodDelete, "/v1/pins", http.MethodGet},
		{http.MethodGet, "/v1/pins/" + cid, "DELETE, PUT"},
		{http.MethodPost, "/v1/pins/" + cid, "DELETE, PUT"},
		{http.MethodPatch, "/v1/pins/" + cid, "DELETE, PUT"},
		{http.MethodGet, "/v1/gc", http.MethodPost},
		{http.MethodPut, "/v1/gc", http.MethodPost},
		{http.MethodDelete, "/v1/gc", http.MethodPost},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, allow, tc.allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestConcurrentPinsAndGCAreLinearizable(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, bytes.Repeat([]byte("race-"), 10000))

	const writers = 8
	const rounds = 25
	var wg sync.WaitGroup
	var pinMu sync.Mutex
	pinSucceeded := false
	for i := 0; i < writers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				rec := putPinRaw(t, h, obj.CID, `{}`, "application/json")
				switch rec.Code {
				case http.StatusCreated, http.StatusOK:
					pinMu.Lock()
					pinSucceeded = true
					pinMu.Unlock()
				case http.StatusNotFound:
					assertErrorCode(t, rec, "object_not_found")
				default:
					t.Errorf("pin status = %d, body = %s", rec.Code, rec.Body.String())
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				resp := runGC(t, h, "")
				for _, c := range resp.CIDs {
					if c == obj.CID {
						pinMu.Lock()
						pinned := pinSucceeded
						pinMu.Unlock()
						if pinned {
							t.Errorf("gc deleted %s after a pin had succeeded", c)
						}
					}
				}
			}
		}()
	}
	wg.Wait()

	// Final state: either a permanent pin exists and protects the object, or
	// gc won the race first and the object is permanently gone.
	if pinSucceeded {
		pins := listPins(t, h)
		if len(pins) != 1 || pins[0].CID != obj.CID {
			t.Fatalf("after successful pin, pins = %+v", pins)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("pinned object missing after race, status = %d", rec.Code)
		}
	} else {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("all pins lost the race, object should be gone, status = %d", rec.Code)
		}
	}
}

func TestReadersObserveWholeObjectOrNothing(t *testing.T) {
	h := Handler()
	payload := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 100000)
	obj := uploadObject(t, h, payload)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+obj.CID, nil))
				switch rec.Code {
				case http.StatusOK:
					if !bytes.Equal(rec.Body.Bytes(), payload) {
						t.Errorf("reader observed %d bytes, want %d", rec.Body.Len(), len(payload))
						return
					}
				case http.StatusNotFound:
					// The standard error document is the only body a 404
					// may carry — never object bytes or a partial manifest.
					if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
						t.Errorf("404 Content-Type = %q, want application/json", ct)
						return
					}
					assertErrorCode(t, rec, "object_not_found")
				default:
					t.Errorf("reader status = %d", rec.Code)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		runGC(t, h, "")
	}()
	wg.Wait()
}
