package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func makeSealedKey(t *testing.T) (string, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key), key
}

func postSealed(t *testing.T, h http.Handler, body []byte, contentType, keyHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sealed-objects", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if keyHeader != "" {
		req.Header.Set("X-CidVault-Key", keyHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sealedRequest(t *testing.T, h http.Handler, method, target, keyHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if keyHeader != "" {
		req.Header.Set("X-CidVault-Key", keyHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSealedResponse(t *testing.T, rec *httptest.ResponseRecorder) sealedResponse {
	t.Helper()
	var resp sealedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return resp
}

// expectedSealedCID recomputes the sealed content identifier from the
// documented inputs: associated data, nonce and tag-carrying ciphertext.
func expectedSealedCID(size int, nonce, ciphertext []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "cidvault-sealed-v1\nsize:%d\n", size)
	h.Write(nonce)
	h.Write(ciphertext)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func TestSealedRoundTrip(t *testing.T) {
	h := Handler()
	keyHeader, key := makeSealedKey(t)
	payload := make([]byte, 100000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	rec := postSealed(t, h, payload, "application/octet-stream", keyHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeSealedResponse(t, rec)
	if resp.Size != len(payload) || resp.Algorithm != "AES-256-GCM" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if !validCID(resp.CID) {
		t.Fatalf("cid %q is not a valid cid", resp.CID)
	}

	// The envelope is readable without any key and carries enough to
	// verify the CID and decrypt independently.
	envRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID+"/envelope", "")
	if envRec.Code != http.StatusOK {
		t.Fatalf("envelope status = %d, want %d", envRec.Code, http.StatusOK)
	}
	if ct := envRec.Header().Get("Content-Type"); ct != "application/vnd.cidvault.sealed+json" {
		t.Fatalf("envelope Content-Type = %q", ct)
	}
	var env sealedEnvelopeResponse
	if err := json.Unmarshal(envRec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if env.Version != 1 || env.Algorithm != "AES-256-GCM" || env.CID != resp.CID || env.Size != len(payload) {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		t.Fatalf("nonce is not standard Base64 of 12 bytes: %v", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		t.Fatalf("ciphertext is not standard Base64: %v", err)
	}
	if len(ciphertext) != len(payload)+16 {
		t.Fatalf("ciphertext length = %d, want %d", len(ciphertext), len(payload)+16)
	}
	if want := expectedSealedCID(len(payload), nonce, ciphertext); env.CID != want {
		t.Fatalf("cid = %q, want %q", env.CID, want)
	}

	// Independent offline decryption from the envelope alone.
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte(fmt.Sprintf("cidvault-sealed-v1\nsize:%d\n", len(payload)))
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		t.Fatalf("independent decryption failed: %v", err)
	}
	if !bytes.Equal(plain, payload) {
		t.Fatal("independently decrypted bytes differ from payload")
	}

	// Authenticated retrieval returns the exact plaintext.
	getRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, keyHeader)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getRec.Code, http.StatusOK)
	}
	if cl := getRec.Header().Get("Content-Length"); cl != fmt.Sprint(len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(payload))
	}
	if cc := getRec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if !bytes.Equal(getRec.Body.Bytes(), payload) {
		t.Fatal("retrieved bytes differ from uploaded payload")
	}
}

func TestSealedEmptyObject(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)

	rec := postSealed(t, h, nil, "application/octet-stream", keyHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeSealedResponse(t, rec)
	if resp.Size != 0 {
		t.Fatalf("size = %d, want 0", resp.Size)
	}
	getRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, keyHeader)
	if getRec.Code != http.StatusOK || getRec.Body.Len() != 0 {
		t.Fatalf("GET status = %d, body %d bytes", getRec.Code, getRec.Body.Len())
	}
	if cl := getRec.Header().Get("Content-Length"); cl != "0" {
		t.Fatalf("Content-Length = %q, want 0", cl)
	}
}

func TestSealedKeyErrors(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("secret"), "application/octet-stream", keyHeader))

	// Missing key: 401 with the challenge header on POST, GET and DELETE.
	for _, tc := range []struct {
		method string
		target string
	}{
		{http.MethodPost, "/v1/sealed-objects"},
		{http.MethodGet, "/v1/sealed-objects/" + resp.CID},
		{http.MethodDelete, "/v1/sealed-objects/" + resp.CID},
	} {
		var rec *httptest.ResponseRecorder
		if tc.method == http.MethodPost {
			rec = postSealed(t, h, []byte("x"), "application/octet-stream", "")
		} else {
			rec = sealedRequest(t, h, tc.method, tc.target, "")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without key: status = %d, want 401", tc.method, tc.target, rec.Code)
		}
		if got := errorCode(t, rec); got != "key_required" {
			t.Fatalf("%s %s without key: code = %q", tc.method, tc.target, got)
		}
		if wa := rec.Header().Get("WWW-Authenticate"); wa != "CidVaultKey" {
			t.Fatalf("%s %s: WWW-Authenticate = %q, want CidVaultKey", tc.method, tc.target, wa)
		}
	}

	// Malformed or wrong-length keys: 400 invalid_key. The URL-safe
	// alphabet is rejected too: only standard Base64 is accepted.
	urlSafeKey := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
	for _, bad := range []string{
		"!!!not-base64!!!",
		base64.StdEncoding.EncodeToString([]byte("short")),
		base64.StdEncoding.EncodeToString(make([]byte, 64)),
		urlSafeKey,
	} {
		rec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, bad)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_key" {
			t.Fatalf("GET with key %q: status = %d code = %q, want 400 invalid_key",
				bad, rec.Code, errorCode(t, rec))
		}
	}

	// Well-formed but wrong key: 403 access_denied, and the object survives.
	wrongHeader, _ := makeSealedKey(t)
	getRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, wrongHeader)
	if getRec.Code != http.StatusForbidden || errorCode(t, getRec) != "access_denied" {
		t.Fatalf("GET with wrong key: status = %d code = %q, want 403 access_denied",
			getRec.Code, errorCode(t, getRec))
	}
	delRec := sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp.CID, wrongHeader)
	if delRec.Code != http.StatusForbidden || errorCode(t, delRec) != "access_denied" {
		t.Fatalf("DELETE with wrong key: status = %d code = %q, want 403 access_denied",
			delRec.Code, errorCode(t, delRec))
	}
	okRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, keyHeader)
	if okRec.Code != http.StatusOK {
		t.Fatalf("GET after failed attempts: status = %d, want 200", okRec.Code)
	}
}

func TestSealedDelete(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("gone soon"), "application/octet-stream", keyHeader))

	delRec := sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp.CID, keyHeader)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want %d", delRec.Code, http.StatusNoContent)
	}
	if delRec.Body.Len() != 0 {
		t.Fatal("DELETE returned a body")
	}

	// After deletion every view reports sealed_object_not_found.
	for _, tc := range []struct {
		method string
		target string
		key    string
	}{
		{http.MethodGet, "/v1/sealed-objects/" + resp.CID, keyHeader},
		{http.MethodDelete, "/v1/sealed-objects/" + resp.CID, keyHeader},
		{http.MethodGet, "/v1/sealed-objects/" + resp.CID + "/envelope", ""},
	} {
		rec := sealedRequest(t, h, tc.method, tc.target, tc.key)
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "sealed_object_not_found" {
			t.Fatalf("%s %s after delete: status = %d code = %q, want 404 sealed_object_not_found",
				tc.method, tc.target, rec.Code, errorCode(t, rec))
		}
	}
}

func TestSealedRequestErrors(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)

	// Wrong media type.
	rec := postSealed(t, h, []byte("x"), "application/json", keyHeader)
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("POST with json media type: status = %d code = %q, want 415 unsupported_media_type",
			rec.Code, errorCode(t, rec))
	}

	// Oversized body: one byte over the limit, and no state is left behind.
	big := make([]byte, MaxObjectSize+1)
	rec = postSealed(t, h, big, "application/octet-stream", keyHeader)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("oversized POST: status = %d code = %q, want 413 payload_too_large",
			rec.Code, errorCode(t, rec))
	}
	statsRec := sealedRequest(t, h, http.MethodGet, "/v1/storage/stats", "")
	var stats map[string]int
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats["objects"] != 0 || stats["storedBytes"] != 0 {
		t.Fatalf("failed upload left state: %+v", stats)
	}

	// Exactly at the limit succeeds.
	rec = postSealed(t, h, make([]byte, MaxObjectSize), "application/octet-stream", keyHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST at the size limit: status = %d, want 201", rec.Code)
	}

	// Malformed and unknown identifiers.
	rec = sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/not-a-cid", keyHeader)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_cid" {
		t.Fatalf("GET with malformed cid: status = %d code = %q, want 400 invalid_cid",
			rec.Code, errorCode(t, rec))
	}
	missing := "sha256:" + strings.Repeat("0", 64)
	rec = sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+missing, keyHeader)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "sealed_object_not_found" {
		t.Fatalf("GET with unknown cid: status = %d code = %q, want 404 sealed_object_not_found",
			rec.Code, errorCode(t, rec))
	}
	rec = sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+missing+"/envelope", "")
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "sealed_object_not_found" {
		t.Fatalf("envelope with unknown cid: status = %d code = %q, want 404 sealed_object_not_found",
			rec.Code, errorCode(t, rec))
	}

	// Unsupported methods carry the right Allow header.
	rec = sealedRequest(t, h, http.MethodPut, "/v1/sealed-objects", keyHeader)
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
		t.Fatalf("PUT /v1/sealed-objects: status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	rec = sealedRequest(t, h, http.MethodPost, "/v1/sealed-objects/"+missing, keyHeader)
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
		t.Fatalf("POST on sealed object: status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, DELETE" {
		t.Fatalf("Allow = %q, want \"GET, DELETE\"", allow)
	}
	rec = sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+missing+"/envelope", keyHeader)
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" {
		t.Fatalf("DELETE on envelope: status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func TestSealedIsolationFromObjectStore(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)
	payload := []byte("isolated sealed payload")
	resp := decodeSealedResponse(t, postSealed(t, h, payload, "application/octet-stream", keyHeader))

	// Sealed objects do not appear in storage statistics.
	statsRec := sealedRequest(t, h, http.MethodGet, "/v1/storage/stats", "")
	var stats map[string]int
	if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats["objects"] != 0 || stats["logicalBytes"] != 0 || stats["blocks"] != 0 || stats["storedBytes"] != 0 {
		t.Fatalf("sealed object leaked into storage stats: %+v", stats)
	}

	// Nor in the audit log.
	auditRec := sealedRequest(t, h, http.MethodGet, "/v1/audit/events", "")
	if strings.Contains(auditRec.Body.String(), resp.CID) {
		t.Fatal("sealed object leaked into the audit log")
	}

	// Nor in the plain object namespace.
	objRec := sealedRequest(t, h, http.MethodGet, "/v1/objects/"+resp.CID, "")
	if objRec.Code != http.StatusNotFound || errorCode(t, objRec) != "object_not_found" {
		t.Fatalf("sealed cid in plain namespace: status = %d, want 404 object_not_found", objRec.Code)
	}

	// Garbage collection leaves sealed objects untouched.
	gcReq := httptest.NewRequest(http.MethodPost, "/v1/gc", nil)
	gcRec := httptest.NewRecorder()
	h.ServeHTTP(gcRec, gcReq)
	if gcRec.Code != http.StatusOK {
		t.Fatalf("gc status = %d", gcRec.Code)
	}
	getRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, keyHeader)
	if getRec.Code != http.StatusOK || !bytes.Equal(getRec.Body.Bytes(), payload) {
		t.Fatalf("sealed object after gc: status = %d", getRec.Code)
	}
}

func TestSealedConcurrentDelete(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("race me"), "application/octet-stream", keyHeader))

	const deleters = 8
	codes := make([]int, deleters)
	var wg sync.WaitGroup
	for i := 0; i < deleters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp.CID, keyHeader)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	noContent, notFound := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusNoContent:
			noContent++
		case http.StatusNotFound:
			notFound++
		default:
			t.Fatalf("unexpected DELETE status %d", c)
		}
	}
	if noContent != 1 || notFound != deleters-1 {
		t.Fatalf("concurrent deletes: %d succeeded, %d not found; want exactly 1 success", noContent, notFound)
	}
}

func TestSealedConcurrentReadAndDelete(t *testing.T) {
	h := Handler()
	keyHeader, _ := makeSealedKey(t)
	payload := make([]byte, 300000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	resp := decodeSealedResponse(t, postSealed(t, h, payload, "application/octet-stream", keyHeader))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, keyHeader)
			switch rec.Code {
			case http.StatusOK:
				if !bytes.Equal(rec.Body.Bytes(), payload) {
					t.Error("concurrent read returned partial or corrupt plaintext")
				}
			case http.StatusNotFound:
			default:
				t.Errorf("concurrent read returned status %d", rec.Code)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp.CID, keyHeader)
	}()
	wg.Wait()
}
