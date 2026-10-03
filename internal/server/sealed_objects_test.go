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

// sealTestKey returns a deterministic 32-byte key derived from seed.
func sealTestKey(seed byte) []byte {
	k := make([]byte, sealedKeySize)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func keyHeader(v []byte) string {
	return base64.StdEncoding.EncodeToString(v)
}

func postSealed(t *testing.T, h http.Handler, body, key []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sealed-objects", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != nil {
		req.Header.Set(sealedKeyHeader, keyHeader(key))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sealedRequest(t *testing.T, h http.Handler, method, path string, key []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if key != nil {
		req.Header.Set(sealedKeyHeader, keyHeader(key))
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

func TestSealedRoundTrip(t *testing.T) {
	h := Handler()
	key := sealTestKey(1)
	payload := []byte("the quick brown fox jumps over the lazy dog")

	rec := postSealed(t, h, payload, key, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeSealedResponse(t, rec)
	if resp.Algorithm != "AES-256-GCM" || resp.Size != len(payload) || !validCID(resp.CID) {
		t.Fatalf("unexpected response: %+v", resp)
	}

	get := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, key)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: %s", get.Code, http.StatusOK, get.Body.String())
	}
	if ct := get.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cl := get.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(payload)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(payload))
	}
	if cc := get.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if !bytes.Equal(get.Body.Bytes(), payload) {
		t.Fatal("decrypted bytes differ from the original plaintext")
	}
}

func TestSealedEmptyBody(t *testing.T) {
	h := Handler()
	key := sealTestKey(2)
	rec := postSealed(t, h, nil, key, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeSealedResponse(t, rec)
	if resp.Size != 0 {
		t.Fatalf("size = %d, want 0", resp.Size)
	}
	get := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, key)
	if get.Code != http.StatusOK || get.Body.Len() != 0 {
		t.Fatalf("empty GET: status = %d body = %q", get.Code, get.Body.String())
	}
	if cl := get.Header().Get("Content-Length"); cl != "0" {
		t.Fatalf("Content-Length = %q, want 0", cl)
	}
}

func TestSealedFreshNonceYieldsDistinctCID(t *testing.T) {
	h := Handler()
	key := sealTestKey(3)
	payload := []byte("same plaintext, same key")
	first := decodeSealedResponse(t, postSealed(t, h, payload, key, "application/octet-stream"))
	second := decodeSealedResponse(t, postSealed(t, h, payload, key, "application/octet-stream"))
	if first.CID == second.CID {
		t.Fatal("two seals of identical content must differ because the nonce is fresh")
	}
	// Both envelopes remain independently readable.
	for _, cid := range []string{first.CID, second.CID} {
		if get := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+cid, key); get.Code != http.StatusOK ||
			!bytes.Equal(get.Body.Bytes(), payload) {
			t.Fatalf("cid %s not readable: status %d", cid, get.Code)
		}
	}
}

func TestSealedCIDMatchesDocumentedConstruction(t *testing.T) {
	h := Handler()
	key := sealTestKey(4)
	payload := bytes.Repeat([]byte("cid-bound-"), 100)
	resp := decodeSealedResponse(t, postSealed(t, h, payload, key, "application/octet-stream"))

	envRec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID+"/envelope", nil)
	if envRec.Code != http.StatusOK {
		t.Fatalf("envelope status = %d, want %d", envRec.Code, http.StatusOK)
	}
	if ct := envRec.Header().Get("Content-Type"); ct != "application/vnd.cidvault.sealed+json" {
		t.Fatalf("envelope Content-Type = %q, want application/vnd.cidvault.sealed+json", ct)
	}
	var env sealedEnvelope
	if err := json.Unmarshal(envRec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if env.Version != 1 || env.Algorithm != "AES-256-GCM" || env.CID != resp.CID || env.Size != len(payload) {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		t.Fatalf("nonce decode: %v len=%d", err, len(nonce))
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		t.Fatalf("ciphertext decode: %v", err)
	}
	// Recompute the CID exactly as specified.
	aad := []byte(fmt.Sprintf("cidvault-sealed-v1\nsize:%d\n", len(payload)))
	hsh := sha256.New()
	hsh.Write(aad)
	hsh.Write(nonce)
	hsh.Write(ciphertext)
	wantCID := "sha256:" + hex.EncodeToString(hsh.Sum(nil))
	if env.CID != wantCID {
		t.Fatalf("envelope cid = %q, want %q", env.CID, wantCID)
	}

	// Independently decrypt the envelope, without touching service code.
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		t.Fatalf("independent decryption failed: %v", err)
	}
	if !bytes.Equal(plaintext, payload) {
		t.Fatal("independently decrypted plaintext differs")
	}

	// A tampered ciphertext must fail GCM authentication on read.
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xff
	obj := &sealedObject{size: len(payload), nonce: nonce, ciphertext: tampered}
	if _, ok := openSealed(key, obj); ok {
		t.Fatal("tampered ciphertext authenticated")
	}
}

func TestSealedEnvelopeNeedsNoKey(t *testing.T) {
	h := Handler()
	key := sealTestKey(5)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("opaque"), key, "application/octet-stream"))
	// Explicitly no X-CidVault-Key header.
	req := httptest.NewRequest(http.MethodGet, "/v1/sealed-objects/"+resp.CID+"/envelope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyless envelope status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSealedWrongKeyAndMissingKey(t *testing.T) {
	h := Handler()
	key := sealTestKey(6)
	other := sealTestKey(7)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("secret"), key, "application/octet-stream"))

	// Missing key on GET.
	rec := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "key_required")
	if wwa := rec.Header().Get("WWW-Authenticate"); wwa != "CidVaultKey" {
		t.Fatalf("WWW-Authenticate = %q, want CidVaultKey", wwa)
	}

	// Wrong key.
	rec = sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, other)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong key status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertErrorCode(t, rec, "access_denied")
	if wwa := rec.Header().Get("WWW-Authenticate"); wwa != "" {
		t.Fatalf("403 must not carry WWW-Authenticate, got %q", wwa)
	}

	// Wrong key must not delete.
	rec = sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp.CID, other)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-key delete status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertErrorCode(t, rec, "access_denied")
	if get := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, key); get.Code != http.StatusOK {
		t.Fatalf("object should survive a forbidden delete, got %d", get.Code)
	}
}

func TestSealedInvalidKey(t *testing.T) {
	h := Handler()
	cid := "sha256:" + strings.Repeat("a", 64)
	invalid := []string{
		"not-base64!!",                   // illegal alphabet
		"AAAA",                           // 3 bytes
		strings.Repeat("A", 44),          // 33 bytes
		strings.Repeat("A", 40) + "AB==", // 32 decoded bytes but non-zero padding bits
		strings.Repeat("A", 43),          // 32 bytes but missing padding
	}
	doRequest := func(method, path string, hdr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(sealedKeyHeader, hdr)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Body = http.NoBody
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, hdr := range invalid {
		rec := doRequest(http.MethodPost, "/v1/sealed-objects", hdr)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST key %q: status = %d, want %d", hdr, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_key")
		rec = doRequest(http.MethodGet, "/v1/sealed-objects/"+cid, hdr)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET key %q: status = %d, want %d", hdr, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_key")
	}
	// A well-formed 16-byte key is invalid_key, not an auth failure.
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	rec := doRequest(http.MethodGet, "/v1/sealed-objects/"+cid, short)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("16-byte key status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertErrorCode(t, rec, "invalid_key")
}

func TestSealedDeleteLifecycle(t *testing.T) {
	h := Handler()
	key := sealTestKey(8)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("ephemeral"), key, "application/octet-stream"))
	path := "/v1/sealed-objects/" + resp.CID

	del := sealedRequest(t, h, http.MethodDelete, path, key)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", del.Code, http.StatusNoContent)
	}
	if body := del.Body.String(); body != "" {
		t.Fatalf("204 must have no body, got %q", body)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := sealedRequest(t, h, method, path, key)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s after delete: status = %d, want %d", method, rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "sealed_object_not_found")
	}
	// The envelope disappears together with the object.
	if rec := sealedRequest(t, h, http.MethodGet, path+"/envelope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("envelope after delete: status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// Delete without a key is 401 even after deletion... here before:
	resp2 := decodeSealedResponse(t, postSealed(t, h, []byte("second"), key, "application/octet-stream"))
	rec := sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+resp2.CID, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("keyless delete status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertErrorCode(t, rec, "key_required")
}

func TestSealedCIDAndNotFoundErrors(t *testing.T) {
	h := Handler()
	key := sealTestKey(9)
	for _, path := range []string{
		"/v1/sealed-objects/not-a-cid",
		"/v1/sealed-objects/sha256:" + strings.Repeat("A", 64),
		"/v1/sealed-objects/sha256:" + strings.Repeat("0", 63),
	} {
		get := sealedRequest(t, h, http.MethodGet, path, key)
		if get.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want %d", path, get.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, get, "invalid_cid")
		env := sealedRequest(t, h, http.MethodGet, path+"/envelope", nil)
		if env.Code != http.StatusBadRequest {
			t.Fatalf("envelope %s: status = %d, want %d", path, env.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, env, "invalid_cid")
	}
	missing := "sha256:" + strings.Repeat("f", 64)
	for _, rec := range []*httptest.ResponseRecorder{
		sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+missing, key),
		sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+missing+"/envelope", nil),
		sealedRequest(t, h, http.MethodDelete, "/v1/sealed-objects/"+missing, key),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing object status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		assertErrorCode(t, rec, "sealed_object_not_found")
	}
}

func TestSealedMediaTypeAndSizeLimits(t *testing.T) {
	h := Handler()
	key := sealTestKey(10)

	for _, ct := range []string{"", "text/plain", "application/json"} {
		rec := postSealed(t, h, []byte("x"), key, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}

	over := make([]byte, MaxObjectSize+1)
	rec := postSealed(t, h, over, key, "application/octet-stream; charset=binary")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	assertErrorCode(t, rec, "payload_too_large")

	exact := make([]byte, MaxObjectSize)
	if _, err := rand.Read(exact); err != nil {
		t.Fatal(err)
	}
	rec = postSealed(t, h, exact, key, "application/octet-stream")
	if rec.Code != http.StatusCreated {
		t.Fatalf("exact-limit status = %d, want %d", rec.Code, http.StatusCreated)
	}
	resp := decodeSealedResponse(t, rec)
	if resp.Size != MaxObjectSize {
		t.Fatalf("size = %d, want %d", resp.Size, MaxObjectSize)
	}
	get := sealedRequest(t, h, http.MethodGet, "/v1/sealed-objects/"+resp.CID, key)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), exact) {
		t.Fatalf("exact-limit readback failed: status %d", get.Code)
	}
}

func TestSealedMethodNotAllowed(t *testing.T) {
	h := Handler()
	key := sealTestKey(11)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("m"), key, "application/octet-stream"))
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodGet, "/v1/sealed-objects", http.MethodPost},
		{http.MethodPut, "/v1/sealed-objects", http.MethodPost},
		{http.MethodPost, "/v1/sealed-objects/" + resp.CID, "GET, DELETE"},
		{http.MethodPut, "/v1/sealed-objects/" + resp.CID, "GET, DELETE"},
		{http.MethodDelete, "/v1/sealed-objects/" + resp.CID + "/envelope", http.MethodGet},
		{http.MethodPost, "/v1/sealed-objects/" + resp.CID + "/envelope", http.MethodGet},
	}
	for _, tc := range cases {
		rec := sealedRequest(t, h, tc.method, tc.path, key)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != tc.allow {
			t.Fatalf("%s %s: Allow = %q, want %q", tc.method, tc.path, allow, tc.allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestSealedConcurrentDelete(t *testing.T) {
	h := Handler()
	key := sealTestKey(12)
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("race"), key, "application/octet-stream"))
	path := "/v1/sealed-objects/" + resp.CID

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = sealedRequest(t, h, http.MethodDelete, path, key).Code
		}(i)
	}
	wg.Wait()
	deleted := 0
	for _, c := range codes {
		if c != http.StatusNoContent && c != http.StatusNotFound {
			t.Fatalf("concurrent delete code = %d, want 204 or 404", c)
		}
		if c == http.StatusNoContent {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("successful deletes = %d, want 1", deleted)
	}
}

func TestSealedConcurrentReadAndDelete(t *testing.T) {
	h := Handler()
	key := sealTestKey(13)
	payload := bytes.Repeat([]byte("ab"), 5000)
	resp := decodeSealedResponse(t, postSealed(t, h, payload, key, "application/octet-stream"))
	path := "/v1/sealed-objects/" + resp.CID

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				sealedRequest(t, h, http.MethodDelete, path, key)
				return
			}
			rec := sealedRequest(t, h, http.MethodGet, path, key)
			switch rec.Code {
			case http.StatusOK:
				if !bytes.Equal(rec.Body.Bytes(), payload) {
					t.Errorf("partial or corrupted read observed")
				}
			case http.StatusNotFound:
			default:
				t.Errorf("read code = %d, want 200 or 404", rec.Code)
			}
		}(i)
	}
	wg.Wait()
}

func TestSealedObjectsAreIsolated(t *testing.T) {
	h := Handler()
	key := sealTestKey(14)
	before := getStorageSnapshot(t, h)
	postSealed(t, h, []byte("not counted"), key, "application/octet-stream")
	after := getStorageSnapshot(t, h)
	if before != after {
		t.Fatalf("sealed objects changed storage stats: %+v -> %+v", before, after)
	}

	// Sealed objects are not reachable through the plaintext routes.
	resp := decodeSealedResponse(t, postSealed(t, h, []byte("hidden"), key, "application/octet-stream"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/objects/"+resp.CID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("plaintext GET of sealed cid: status = %d, want 404", rec.Code)
	}
}

func getStorageSnapshot(t *testing.T, h http.Handler) statsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d", rec.Code)
	}
	var st statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}
