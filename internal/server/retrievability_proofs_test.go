package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func postProof(t *testing.T, h http.Handler, cid string, body string, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/retrievability-proofs/"+cid, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validNonce(t *testing.T, size int) string {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// expectedProofSelection independently recomputes the documented sampling:
// SHA-256 over the newline-separated transcript, unsigned lexicographic
// digest order with index tie-break, first samples positions, index order.
func expectedProofSelection(rootCID, nonce string, positions int, samples int) []int {
	type pos struct {
		index  int
		digest [sha256.Size]byte
	}
	all := make([]pos, positions)
	for i := 0; i < positions; i++ {
		transcript := "cidvault-retrievability-v1\n" + rootCID + "\n" + nonce + "\n" + strconv.Itoa(i) + "\n"
		all[i] = pos{index: i, digest: sha256.Sum256([]byte(transcript))}
	}
	sort.Slice(all, func(a, b int) bool {
		if c := bytes.Compare(all[a].digest[:], all[b].digest[:]); c != 0 {
			return c < 0
		}
		return all[a].index < all[b].index
	})
	if samples > len(all) {
		samples = len(all)
	}
	indices := make([]int, 0, samples)
	for _, p := range all[:samples] {
		indices = append(indices, p.index)
	}
	sort.Ints(indices)
	return indices
}

func decodeProofResponse(t *testing.T, rec *httptest.ResponseRecorder) proofResponse {
	t.Helper()
	var resp proofResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("proof response is not JSON: %v", err)
	}
	return resp
}

func TestRetrievabilityProofRoundTrip(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize*3+7)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := uploadObject(t, h, payload)
	nonce := validNonce(t, 32)

	rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":3}`, nonce), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeProofResponse(t, rec)
	if resp.CID != obj.CID || resp.Nonce != nonce || resp.RequestedSamples != 3 {
		t.Fatalf("unexpected proof header: %+v", resp)
	}
	wantIndices := expectedProofSelection(obj.CID, nonce, len(obj.Chunks), 3)
	if len(resp.Entries) != len(wantIndices) {
		t.Fatalf("entries = %d, want %d", len(resp.Entries), len(wantIndices))
	}
	for i, entry := range resp.Entries {
		if entry.Index != wantIndices[i] {
			t.Fatalf("entry %d index = %d, want %d", i, entry.Index, wantIndices[i])
		}
		if entry.CID != obj.Chunks[entry.Index] {
			t.Fatalf("entry %d cid = %q, want manifest cid %q", i, entry.CID, obj.Chunks[entry.Index])
		}
		data, err := base64.StdEncoding.DecodeString(entry.Data)
		if err != nil {
			t.Fatalf("entry %d data is not standard Base64: %v", i, err)
		}
		chunk := payload[entry.Index*ChunkSize:]
		if len(chunk) > ChunkSize {
			chunk = chunk[:ChunkSize]
		}
		if !bytes.Equal(data, chunk) {
			t.Fatalf("entry %d data does not match chunk bytes", i)
		}
		if got := chunkCID(data); got != entry.CID {
			t.Fatalf("entry %d recomputed cid = %q, want %q", i, got, entry.CID)
		}
	}
}

func TestRetrievabilityProofSamplesClampedToManifest(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("small body, one chunk"))
	nonce := validNonce(t, 16)

	rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":16}`, nonce), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp := decodeProofResponse(t, rec)
	if resp.RequestedSamples != 16 {
		t.Fatalf("requestedSamples = %d, want 16", resp.RequestedSamples)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Index != 0 {
		t.Fatalf("entries = %+v, want the single manifest position", resp.Entries)
	}
}

func TestRetrievabilityProofEmptyObject(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, nil)
	nonce := validNonce(t, 64)

	rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":4}`, nonce), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("entries should encode as [], got %s", rec.Body.String())
	}
	resp := decodeProofResponse(t, rec)
	if len(resp.Entries) != 0 {
		t.Fatalf("entries = %+v, want empty", resp.Entries)
	}
}

func TestRetrievabilityProofDuplicateChunksSampledPerPosition(t *testing.T) {
	h := Handler()
	chunk := bytes.Repeat([]byte("dedup"), ChunkSize/5+1)
	chunk = chunk[:ChunkSize]
	payload := append(append([]byte{}, chunk...), chunk...) // two identical chunks
	obj := uploadObject(t, h, payload)
	if len(obj.Chunks) != 2 || obj.Chunks[0] != obj.Chunks[1] {
		t.Fatalf("expected two identical manifest positions, got %+v", obj.Chunks)
	}
	nonce := validNonce(t, 24)

	rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":2}`, nonce), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	resp := decodeProofResponse(t, rec)
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %d, want both positions sampled", len(resp.Entries))
	}
	if resp.Entries[0].Index != 0 || resp.Entries[1].Index != 1 {
		t.Fatalf("entries should cover positions 0 and 1 in order: %+v", resp.Entries)
	}
	for _, entry := range resp.Entries {
		if entry.CID != obj.Chunks[0] {
			t.Fatalf("entry %d cid = %q, want shared chunk cid", entry.Index, entry.CID)
		}
	}
}

func TestRetrievabilityProofNonceBoundaries(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("nonce boundary object"))
	for _, size := range []int{16, 64} {
		nonce := validNonce(t, size)
		rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce), "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("nonce size %d: status = %d, want %d", size, rec.Code, http.StatusOK)
		}
	}
	for _, size := range []int{15, 65} {
		nonce := validNonce(t, size)
		rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce), "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("nonce size %d: status = %d, want %d", size, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_nonce")
	}
}

func TestRetrievabilityProofInvalidNonce(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("nonce validation object"))
	canonical := base64.StdEncoding.EncodeToString(make([]byte, 16)) // "AAAA...AA=="
	nonCanonical := canonical[:len(canonical)-3] + "B=="             // same bytes, pad bits set
	cases := []string{
		nonCanonical, // non-canonical padding bits
		base64.RawStdEncoding.EncodeToString(make([]byte, 16)),            // missing padding
		base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 16)), // URL-safe alphabet
		strings.Repeat("!", 24), // illegal characters
		"",                      // empty
	}
	for _, nonce := range cases {
		rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce), "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("nonce %q: status = %d, want %d", nonce, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_nonce")
	}
}

func TestRetrievabilityProofSampleCountValidation(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("sample count object"))
	nonce := validNonce(t, 16)
	for _, samples := range []string{"0", "17", "-1", "999999999999999999999999"} {
		body := fmt.Sprintf(`{"nonce":%q,"samples":%s}`, nonce, samples)
		rec := postProof(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("samples %s: status = %d, want %d", samples, rec.Code, http.StatusUnprocessableEntity)
		}
		assertErrorCode(t, rec, "invalid_sample_count")
	}
	for _, samples := range []string{`"3"`, "2.5", "1e2", "true", "null", "[1]", "{}"} {
		body := fmt.Sprintf(`{"nonce":%q,"samples":%s}`, nonce, samples)
		rec := postProof(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("samples %s: status = %d, want %d", samples, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestRetrievabilityProofStrictBody(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("strict body object"))
	nonce := validNonce(t, 16)
	bodies := []string{
		"",                                 // empty
		"   ",                              // whitespace only
		"not json",                         // garbage
		`[1,2]`,                            // not an object
		`null`,                             // not an object
		`{}`,                               // missing fields
		fmt.Sprintf(`{"nonce":%q}`, nonce), // missing samples
		`{"samples":2}`,                    // missing nonce
		fmt.Sprintf(`{"nonce":%q,"samples":2,"extra":1}`, nonce),         // unknown field
		fmt.Sprintf(`{"nonce":%q,"nonce":%q,"samples":2}`, nonce, nonce), // duplicate field
		fmt.Sprintf(`{"nonce":%q,"samples":2} {"a":1}`, nonce),           // trailing value
		fmt.Sprintf(`{"nonce":%q,"samples":2} trailing`, nonce),          // trailing garbage
		`{"nonce":16,"samples":2}`,                                       // nonce wrong type
		`{"nonce":null,"samples":2}`,                                     // nonce null
		fmt.Sprintf(`{"nonce":%q,"samples":2}`, nonce) + "\n\n{}",        // second document
	}
	for _, body := range bodies {
		rec := postProof(t, h, obj.CID, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_request")
	}
}

func TestRetrievabilityProofMediaType(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("media type object"))
	nonce := validNonce(t, 16)
	body := fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce)
	for _, ct := range []string{"", "text/plain", "application/octet-stream", "application/jsonx"} {
		rec := postProof(t, h, obj.CID, body, ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: status = %d, want %d", ct, rec.Code, http.StatusUnsupportedMediaType)
		}
		assertErrorCode(t, rec, "unsupported_media_type")
	}
	rec := postProof(t, h, obj.CID, body, "application/json; charset=utf-8")
	if rec.Code != http.StatusOK {
		t.Fatalf("parameterized JSON media type: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRetrievabilityProofCIDAndExistence(t *testing.T) {
	h := Handler()
	nonce := validNonce(t, 16)
	body := fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce)

	for _, cid := range []string{"not-a-cid", "sha256:ABC", "sha256:" + strings.Repeat("g", 64)} {
		rec := postProof(t, h, cid, body, "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("cid %q: status = %d, want %d", cid, rec.Code, http.StatusBadRequest)
		}
		assertErrorCode(t, rec, "invalid_cid")
	}

	missing := "sha256:" + strings.Repeat("0", 64)
	rec := postProof(t, h, missing, body, "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing object: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	assertErrorCode(t, rec, "object_not_found")
}

func TestRetrievabilityProofMethodNotAllowed(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("method object"))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/retrievability-proofs/"+obj.CID, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
		assertErrorCode(t, rec, "method_not_allowed")
	}
}

func TestRetrievabilityProofIsReadOnly(t *testing.T) {
	h := Handler()
	obj := uploadObject(t, h, []byte("read only object"))

	statsBefore := httptest.NewRecorder()
	h.ServeHTTP(statsBefore, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	eventsBefore := httptest.NewRecorder()
	h.ServeHTTP(eventsBefore, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))

	nonce := validNonce(t, 16)
	rec := postProof(t, h, obj.CID, fmt.Sprintf(`{"nonce":%q,"samples":1}`, nonce), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	statsAfter := httptest.NewRecorder()
	h.ServeHTTP(statsAfter, httptest.NewRequest(http.MethodGet, "/v1/storage/stats", nil))
	if statsBefore.Body.String() != statsAfter.Body.String() {
		t.Fatalf("stats changed: %s -> %s", statsBefore.Body.String(), statsAfter.Body.String())
	}
	eventsAfter := httptest.NewRecorder()
	h.ServeHTTP(eventsAfter, httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil))
	if eventsBefore.Body.String() != eventsAfter.Body.String() {
		t.Fatal("proof request appended audit events")
	}
	pins := httptest.NewRecorder()
	h.ServeHTTP(pins, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	if !strings.Contains(pins.Body.String(), `"pins":[]`) {
		t.Fatalf("proof request created a pin: %s", pins.Body.String())
	}
}
