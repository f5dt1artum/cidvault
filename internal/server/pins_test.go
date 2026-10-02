package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func upload(t *testing.T, h http.Handler, body []byte) objectResponse {
	t.Helper()
	rec := postBody(t, h, body, "application/octet-stream")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 201 or 200", rec.Code)
	}
	return decodeObjectResponse(t, rec)
}

func putPin(t *testing.T, h http.Handler, cid, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/pins/"+cid, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodePinResponse(t *testing.T, rec *httptest.ResponseRecorder) pinResponse {
	t.Helper()
	var resp pinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("pin response is not JSON: %v", err)
	}
	return resp
}

func listPins(t *testing.T, h http.Handler) pinListResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/pins status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp pinListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("pin list is not JSON: %v", err)
	}
	return resp
}

func runGC(t *testing.T, h http.Handler, query string) (int, gcResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/gc"+query, nil))
	var resp gcResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("gc response is not JSON: %v", err)
		}
	}
	return rec.Code, resp
}

func TestPinCreateUpdateListDelete(t *testing.T) {
	h := Handler()
	a := upload(t, h, []byte("alpha"))
	b := upload(t, h, []byte("beta"))

	// Create a permanent pin.
	rec := putPin(t, h, a.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodePinResponse(t, rec)
	if resp.CID != a.CID || resp.ExpiresAt != nil {
		t.Fatalf("unexpected pin response: %+v", resp)
	}

	// Null expiresAt is also permanent.
	rec = putPin(t, h, b.CID, `{"expiresAt":null}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("null-expiry create status = %d, want %d", rec.Code, http.StatusCreated)
	}

	// Updating an in-force pin returns 200.
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec = putPin(t, h, a.CID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp = decodePinResponse(t, rec)
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(time.Now().Add(time.Hour).UTC().Truncate(time.Second)) {
		t.Fatalf("unexpected updated expiry: %+v", resp.ExpiresAt)
	}

	// Listing returns both pins, sorted by cid, permanent ones null.
	list := listPins(t, h)
	if len(list.Pins) != 2 {
		t.Fatalf("listed %d pins, want 2", len(list.Pins))
	}
	if list.Pins[0].CID >= list.Pins[1].CID {
		t.Fatalf("pins not sorted by cid: %+v", list.Pins)
	}
	byCID := map[string]pinResponse{}
	for _, p := range list.Pins {
		byCID[p.CID] = p
	}
	if byCID[a.CID].ExpiresAt == nil {
		t.Fatal("updated pin should carry an expiration")
	}
	if byCID[b.CID].ExpiresAt != nil {
		t.Fatal("permanent pin should list expiresAt as null")
	}

	// Delete, then delete again.
	req := httptest.NewRequest(http.MethodDelete, "/v1/pins/"+a.CID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+a.CID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "pin_not_found")

	if list := listPins(t, h); len(list.Pins) != 1 || list.Pins[0].CID != b.CID {
		t.Fatalf("unexpected pins after delete: %+v", list.Pins)
	}
}

func TestPinEmptyListEncodesAsArray(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"pins":[]`) {
		t.Fatalf("pins should encode as [], got %s", rec.Body.String())
	}
}

func TestPinValidationErrors(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("validate me"))

	// Wrong or missing media type.
	for _, ct := range []string{"text/plain", "application/octet-stream", ""} {
		rec := putPin(t, h, obj.CID, `{}`, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	// Malformed JSON, unknown fields, wrong types, trailing data.
	for _, body := range []string{
		`{`,
		`{"expiresAt":`,
		`{"unknown":1}`,
		`{"expiresAt":123}`,
		`{} {}`,
		`[1]`,
	} {
		rec := putPin(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}

	// Malformed or non-future expirations.
	for _, body := range []string{
		`{"expiresAt":"not-a-time"}`,
		`{"expiresAt":"2020-01-01T00:00:00Z"}`,
		fmt.Sprintf(`{"expiresAt":%q}`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
	} {
		rec := putPin(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_expiration")
	}

	// Invalid cid shape.
	rec := putPin(t, h, "not-a-cid", `{}`, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cid status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_cid")

	// Valid cid, missing object.
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = putPin(t, h, missing, `{}`, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")

	// Deleting a pin that never existed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+missing, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing pin status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "pin_not_found")
}

func TestExpiredPinBehavesAsAbsent(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("short lived pin"))

	expiry := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	rec := putPin(t, h, obj.CID, `{"expiresAt":"`+expiry+`"}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if list := listPins(t, h); len(list.Pins) != 1 {
		t.Fatalf("pin should be listed before expiry: %+v", list.Pins)
	}

	time.Sleep(300 * time.Millisecond)

	// Expired pins are not listed.
	if list := listPins(t, h); len(list.Pins) != 0 {
		t.Fatalf("expired pin should not be listed: %+v", list.Pins)
	}
	// Deleting an expired pin reports pin_not_found.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete expired pin status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "pin_not_found")
	// Re-pinning after expiry is a creation, not an update.
	rec = putPin(t, h, obj.CID, `{}`, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-pin after expiry status = %d, want %d", rec.Code, http.StatusCreated)
	}
}

func TestGarbageCollection(t *testing.T) {
	h := Handler()
	pinned := upload(t, h, []byte("pinned"))
	unpinned := upload(t, h, []byte("unpinned"))
	expiring := upload(t, h, []byte("expiring pin"))

	if rec := putPin(t, h, pinned.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d", rec.Code)
	}
	short := time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	if rec := putPin(t, h, expiring.CID, `{"expiresAt":"`+short+`"}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)

	// Dry run previews without deleting.
	code, preview := runGC(t, h, "?dryRun=true")
	if code != http.StatusOK {
		t.Fatalf("dry run status = %d, want %d", code, http.StatusOK)
	}
	if !preview.DryRun || preview.Objects != 2 || preview.Bytes != len("unpinned")+len("expiring pin") {
		t.Fatalf("unexpected dry run response: %+v", preview)
	}
	if len(preview.CIDs) != 2 || preview.CIDs[0] >= preview.CIDs[1] {
		t.Fatalf("dry run cids not sorted: %+v", preview.CIDs)
	}
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+unpinned.CID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("dry run must not delete, GET status = %d", getRec.Code)
	}

	// Real run deletes only the unpinned and the expired-pin objects.
	code, result := runGC(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("gc status = %d, want %d", code, http.StatusOK)
	}
	if result.DryRun || result.Objects != 2 || result.Bytes != preview.Bytes {
		t.Fatalf("unexpected gc response: %+v", result)
	}
	for _, cid := range []string{unpinned.CID, expiring.CID} {
		for _, suffix := range []string{"", "/manifest"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+cid+suffix, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET collected object%s: status = %d, want %d", suffix, rec.Code, http.StatusNotFound)
			}
			assertErrorCode(t, rec, "object_not_found")
		}
	}
	// The pinned object survives.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+pinned.CID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pinned object collected, GET status = %d", rec.Code)
	}

	// Re-uploading collected content yields the original cid as a new object.
	again := upload(t, h, []byte("unpinned"))
	if !again.Created || again.CID != unpinned.CID {
		t.Fatalf("re-upload = %+v, want created=true cid=%q", again, unpinned.CID)
	}

	// The re-uploaded object is itself unpinned, so the next sweep collects
	// exactly it; after that nothing is left.
	_, result = runGC(t, h, "")
	if result.Objects != 1 || result.Bytes != len("unpinned") || len(result.CIDs) != 1 || result.CIDs[0] != unpinned.CID {
		t.Fatalf("second gc should collect only the re-upload: %+v", result)
	}
	_, result = runGC(t, h, "")
	if result.Objects != 0 || result.Bytes != 0 || len(result.CIDs) != 0 {
		t.Fatalf("third gc should be empty: %+v", result)
	}
}

func TestGCRejectsBadDryRun(t *testing.T) {
	h := Handler()
	for _, query := range []string{"?dryRun=false", "?dryRun=1", "?dryRun=yes", "?dryRun="} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/gc"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want %d", query, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestPinAndGCMethodNotAllowed(t *testing.T) {
	h := Handler()
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodPost, "/v1/pins", http.MethodGet},
		{http.MethodDelete, "/v1/pins", http.MethodGet},
		{http.MethodGet, "/v1/pins/" + strings.Repeat("0", 64), "PUT, DELETE"},
		{http.MethodPost, "/v1/pins/" + strings.Repeat("0", 64), "PUT, DELETE"},
		{http.MethodGet, "/v1/gc", http.MethodPost},
		{http.MethodDelete, "/v1/gc", http.MethodPost},
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

func TestGCPinOrderingGuarantee(t *testing.T) {
	h := Handler()
	obj := upload(t, h, []byte("race me"))

	// Pin first: a subsequent gc must keep the object.
	if rec := putPin(t, h, obj.CID, `{}`, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d", rec.Code)
	}
	if _, result := runGC(t, h, ""); result.Objects != 0 {
		t.Fatalf("gc collected a pinned object: %+v", result)
	}

	// Unpin and collect: a subsequent pin must report the object missing.
	req := httptest.NewRequest(http.MethodDelete, "/v1/pins/"+obj.CID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unpin status = %d", rec.Code)
	}
	if _, result := runGC(t, h, ""); result.Objects != 1 {
		t.Fatalf("gc should collect the unpinned object: %+v", result)
	}
	rec = putPin(t, h, obj.CID, `{}`, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pin after gc status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestUploadDoesNotAutoPin(t *testing.T) {
	h := Handler()
	upload(t, h, []byte("no pin by default"))
	if list := listPins(t, h); len(list.Pins) != 0 {
		t.Fatalf("upload must not create pins: %+v", list.Pins)
	}
}
