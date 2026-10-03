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
	"testing"
)

// postProof issues POST /v1/retrievability-proofs/{cid} with the given raw
// body and content type.
func postProof(t *testing.T, h http.Handler, cid string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/retrievability-proofs/"+cid, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// proofBody builds a well-formed request body for the given raw nonce bytes.
func proofBody(t *testing.T, nonce []byte, samples int) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"nonce":   base64.StdEncoding.EncodeToString(nonce),
		"samples": samples,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// expectedProofSelection independently recomputes the documented sampling
// contract: score every manifest position, rank by digest then index, take
// the first take positions and return their indices in ascending order.
func expectedProofSelection(rootCID, nonce string, positions, take int) []int {
	type scored struct {
		index  int
		digest [sha256.Size]byte
	}
	scores := make([]scored, positions)
	for i := range scores {
		input := "cidvault-retrievability-v1\n" + rootCID + "\n" + nonce + "\n" +
			strconv.Itoa(i) + "\n"
		scores[i] = scored{index: i, digest: sha256.Sum256([]byte(input))}
	}
	sort.Slice(scores, func(a, b int) bool {
		if c := bytes.Compare(scores[a].digest[:], scores[b].digest[:]); c != 0 {
			return c < 0
		}
		return scores[a].index < scores[b].index
	})
	if take > positions {
		take = positions
	}
	indices := make([]int, 0, take)
	for _, s := range scores[:take] {
		indices = append(indices, s.index)
	}
	sort.Ints(indices)
	return indices
}

func decodeProofResponse(t *testing.T, rec *httptest.ResponseRecorder) proofResponse {
	t.Helper()
	var resp proofResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return resp
}

func TestProofSelectsDocumentedPositions(t *testing.T) {
	h := Handler()
	// Three distinct chunks plus a repeat of the first, so duplicate blocks
	// occupy two manifest positions.
	chunkA := make([]byte, ChunkSize)
	chunkB := make([]byte, ChunkSize)
	tail := make([]byte, 500)
	for _, buf := range [][]byte{chunkA, chunkB, tail} {
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	payload := append(append(append(append([]byte{}, chunkA...), chunkB...), chunkA...), tail...)
	obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
	if len(obj.Chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(obj.Chunks))
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	canonical := base64.StdEncoding.EncodeToString(nonce)
	rec := postProof(t, h, obj.CID, proofBody(t, nonce, 2), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp := decodeProofResponse(t, rec)
	if resp.CID != obj.CID || resp.Nonce != canonical || resp.RequestedSamples != 2 {
		t.Fatalf("unexpected response header: %+v", resp)
	}
	want := expectedProofSelection(obj.CID, canonical, 4, 2)
	if len(resp.Entries) != len(want) {
		t.Fatalf("entries = %d, want %d", len(resp.Entries), len(want))
	}
	for i, idx := range want {
		entry := resp.Entries[i]
		if entry.Index != idx {
			t.Fatalf("entry %d index = %d, want %d", i, entry.Index, idx)
		}
		if entry.CID != obj.Chunks[idx] {
			t.Fatalf("entry %d cid = %q, want %q", i, entry.CID, obj.Chunks[idx])
		}
		data, err := base64.StdEncoding.DecodeString(entry.Data)
		if err != nil {
			t.Fatalf("entry %d data is not standard Base64: %v", i, err)
		}
		if !bytes.Equal(data, chunkOf(payload, idx)) {
			t.Fatalf("entry %d data does not match chunk %d", i, idx)
		}
		if chunkCID(data) != entry.CID {
			t.Fatalf("entry %d data does not recompute to its cid", i)
		}
		if i > 0 && resp.Entries[i-1].Index >= entry.Index {
			t.Fatalf("entries not ordered by index at %d", i)
		}
	}
}

func TestProofSamplesCappedAtPositions(t *testing.T) {
	h := Handler()
	payload := []byte("small object")
	obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	rec := postProof(t, h, obj.CID, proofBody(t, nonce, 16), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := decodeProofResponse(t, rec)
	if resp.RequestedSamples != 16 {
		t.Fatalf("requestedSamples = %d, want 16", resp.RequestedSamples)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Index != 0 {
		t.Fatalf("entries = %+v, want the single position", resp.Entries)
	}
	data, _ := base64.StdEncoding.DecodeString(resp.Entries[0].Data)
	if !bytes.Equal(data, payload) {
		t.Fatalf("entry data does not match the object body")
	}
}

func TestProofEmptyObject(t *testing.T) {
	h := Handler()
	obj := decodeObjectResponse(t, postBody(t, h, nil, "application/octet-stream"))
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	rec := postProof(t, h, obj.CID, proofBody(t, nonce, 4), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := decodeProofResponse(t, rec)
	if resp.Entries == nil || len(resp.Entries) != 0 {
		t.Fatalf("entries = %+v, want empty array", resp.Entries)
	}
}

func TestProofDuplicatePositionsSampledSeparately(t *testing.T) {
	h := Handler()
	// A body of one full-size chunk repeated twice: two manifest positions,
	// one unique block. Requesting both positions must surface both indices.
	chunk := make([]byte, ChunkSize)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	payload := append(append([]byte{}, chunk...), chunk...)
	obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
	if len(obj.Chunks) != 2 || obj.Chunks[0] != obj.Chunks[1] {
		t.Fatalf("expected two identical manifest positions, got %+v", obj.Chunks)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	rec := postProof(t, h, obj.CID, proofBody(t, nonce, 2), "application/json")
	resp := decodeProofResponse(t, rec)
	if len(resp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].Index != 0 || resp.Entries[1].Index != 1 {
		t.Fatalf("indices = %d,%d, want 0,1", resp.Entries[0].Index, resp.Entries[1].Index)
	}
	for _, e := range resp.Entries {
		if e.CID != obj.Chunks[0] {
			t.Fatalf("entry cid = %q, want %q", e.CID, obj.Chunks[0])
		}
	}
}

func TestProofValidationFailures(t *testing.T) {
	h := Handler()
	obj := decodeObjectResponse(t, postBody(t, h, []byte("data"), "application/octet-stream"))
	validNonce := base64.StdEncoding.EncodeToString(make([]byte, 16))

	cases := []struct {
		name        string
		method      string
		cid         string
		body        string
		contentType string
		status      int
		code        string
		allow       string
	}{
		{name: "method", method: http.MethodGet, cid: obj.CID, status: http.StatusMethodNotAllowed, code: "method_not_allowed", allow: http.MethodPost},
		{name: "bad cid", cid: "sha256:xyz", body: `{"nonce":"` + validNonce + `","samples":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_cid"},
		{name: "no media type", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":1}`, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "wrong media type", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":1}`, contentType: "text/plain", status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "malformed json", cid: obj.CID, body: `{"nonce":`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "trailing json", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":1} {}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "missing nonce", cid: obj.CID, body: `{"samples":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "missing samples", cid: obj.CID, body: `{"nonce":"` + validNonce + `"}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "unknown field", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":1,"x":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "duplicate field", cid: obj.CID, body: `{"nonce":"` + validNonce + `","nonce":"` + validNonce + `","samples":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "nonce wrong type", cid: obj.CID, body: `{"nonce":5,"samples":1}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "samples wrong type", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":"1"}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "samples fractional", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":1.5}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "nonce not base64", cid: obj.CID, body: `{"nonce":"!!!","samples":1}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_nonce"},
		{name: "nonce unpadded", cid: obj.CID, body: `{"nonce":"AQ","samples":1}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_nonce"},
		{name: "nonce non-canonical", cid: obj.CID, body: `{"nonce":"` + nonCanonicalNonce() + `","samples":1}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_nonce"},
		{name: "nonce too short", cid: obj.CID, body: `{"nonce":"` + base64.StdEncoding.EncodeToString(make([]byte, 15)) + `","samples":1}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_nonce"},
		{name: "nonce too long", cid: obj.CID, body: `{"nonce":"` + base64.StdEncoding.EncodeToString(make([]byte, 65)) + `","samples":1}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_nonce"},
		{name: "samples zero", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":0}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_sample_count"},
		{name: "samples too large", cid: obj.CID, body: `{"nonce":"` + validNonce + `","samples":17}`, contentType: "application/json", status: http.StatusUnprocessableEntity, code: "invalid_sample_count"},
		{name: "not found", cid: rootCID(1, []string{chunkCID([]byte("ghost"))}), body: `{"nonce":"` + validNonce + `","samples":1}`, contentType: "application/json", status: http.StatusNotFound, code: "object_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, "/v1/retrievability-proofs/"+tc.cid, bytes.NewReader([]byte(tc.body)))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			if code := errorCode(t, rec); code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if tc.allow != "" && rec.Header().Get("Allow") != tc.allow {
				t.Fatalf("Allow = %q, want %q", rec.Header().Get("Allow"), tc.allow)
			}
		})
	}
}

// nonCanonicalNonce returns a standard-Base64-decodable string whose
// trailing padding bits are non-zero, so it is not the canonical encoding
// of its decoded bytes: 17 zero bytes canonically encode to 23 'A' sextets
// plus "=", and the final sextet carries two zero padding bits; setting
// them ('B') leaves the decoded bytes unchanged but breaks canonical form.
func nonCanonicalNonce() string {
	return "AAAAAAAAAAAAAAAAAAAAAAB="
}

func TestProofIsReadOnly(t *testing.T) {
	h := Handler()
	payload := make([]byte, ChunkSize+7)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))

	statsBefore := getStats(t, h)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	rec := postProof(t, h, obj.CID, proofBody(t, nonce, 2), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if got := getStats(t, h); got != statsBefore {
		t.Fatalf("stats changed: %+v -> %+v", statsBefore, got)
	}
	// No audit event may be appended by the proof endpoint.
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	auditRec := httptest.NewRecorder()
	h.ServeHTTP(auditRec, req)
	var events struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	if err := json.Unmarshal(auditRec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Action != "object.upload" {
		t.Fatalf("audit events = %+v, want only the upload", events.Events)
	}
	// No pin may be established.
	pinRec := httptest.NewRecorder()
	h.ServeHTTP(pinRec, httptest.NewRequest(http.MethodGet, "/v1/pins", nil))
	var pins struct {
		Pins []any `json:"pins"`
	}
	if err := json.Unmarshal(pinRec.Body.Bytes(), &pins); err != nil {
		t.Fatal(err)
	}
	if len(pins.Pins) != 0 {
		t.Fatalf("pins = %+v, want none", pins.Pins)
	}
}

// TestProofConcurrentWithGC exercises the all-or-nothing contract: a proof
// request racing a sweep either sees the whole object or object_not_found.
func TestProofConcurrentWithGC(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := Handler()
		payload := []byte(fmt.Sprintf("payload-%d", i))
		obj := decodeObjectResponse(t, postBody(t, h, payload, "application/octet-stream"))
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		body := proofBody(t, nonce, 1)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/retrievability-proofs/"+obj.CID, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			done <- rec
		}()
		gcReq := httptest.NewRequest(http.MethodPost, "/v1/gc", nil)
		gcRec := httptest.NewRecorder()
		h.ServeHTTP(gcRec, gcReq)
		rec := <-done
		switch rec.Code {
		case http.StatusOK:
			resp := decodeProofResponse(t, rec)
			if len(resp.Entries) != 1 || resp.Entries[0].Index != 0 {
				t.Fatalf("partial response after race: %+v", resp)
			}
		case http.StatusNotFound:
			if code := errorCode(t, rec); code != "object_not_found" {
				t.Fatalf("code = %q, want object_not_found", code)
			}
		default:
			t.Fatalf("status = %d during race", rec.Code)
		}
	}
}
