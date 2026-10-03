package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
)

// sealedAlgorithm is the only seal algorithm accepted by the vault.
const sealedAlgorithm = "AES-256-GCM"

// sealedKeyHeader carries the caller-held Base64 key.
const sealedKeyHeader = "X-CidVault-Key"

// sealedNonceSize is the per-object GCM nonce length.
const sealedNonceSize = 12

// sealedKeySize is the required key length in bytes.
const sealedKeySize = 32

// sealedObject is one encrypted envelope held in process memory. Only the
// nonce and the ciphertext (including its GCM authentication tag) are
// retained; neither the plaintext nor the key outlives a request.
type sealedObject struct {
	size       int
	nonce      []byte
	ciphertext []byte // ciphertext with the GCM tag appended
}

// sealedStore is an in-process envelope store, deliberately separate from
// the plaintext object store: sealed objects take no part in the object
// lifecycle, blocks, pins, garbage collection, storage statistics or the
// audit log. Inserts and deletes happen under the write lock so a reader
// observes either the complete envelope or its absence.
type sealedStore struct {
	mu      sync.RWMutex
	objects map[string]*sealedObject
}

func newSealedStore() *sealedStore {
	return &sealedStore{objects: make(map[string]*sealedObject)}
}

// sealedAAD builds the authenticated data bound to one object: the domain
// line followed by the plaintext length.
func sealedAAD(size int) []byte {
	return []byte(fmt.Sprintf("cidvault-sealed-v1\nsize:%d\n", size))
}

// sealedCID derives the content identifier over the associated data, the
// nonce and the ciphertext (authentication tag included).
func sealedCID(aad, nonce, ciphertext []byte) string {
	h := sha256.New()
	h.Write(aad)
	h.Write(nonce)
	h.Write(ciphertext)
	sum := h.Sum(nil)
	return cidPrefix + hex.EncodeToString(sum)
}

// insert stores the envelope under cid, returning created == false when an
// envelope with the same identifier already exists (its bytes are
// necessarily identical, since the cid commits to all of them).
func (s *sealedStore) insert(cid string, obj *sealedObject) (created bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[cid]; ok {
		return false
	}
	s.objects[cid] = obj
	return true
}

// snapshot returns a copy of the stored envelope pointer for cid, or nil
// when absent. The pointed-to byte slices are never mutated after insert,
// so decryption outside the lock cannot race a concurrent delete.
func (s *sealedStore) snapshot(cid string) *sealedObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objects[cid]
}

// remove deletes the envelope for cid, reporting whether one was present.
// Exactly one of several concurrent callers observes true.
func (s *sealedStore) remove(cid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[cid]; !ok {
		return false
	}
	delete(s.objects, cid)
	return true
}

// sealedResponse is the JSON body returned by POST /v1/sealed-objects.
type sealedResponse struct {
	CID       string `json:"cid"`
	Size      int    `json:"size"`
	Algorithm string `json:"algorithm"`
}

// sealedEnvelope is the keyless JSON disclosure of one stored object.
type sealedEnvelope struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	CID        string `json:"cid"`
	Size       int    `json:"size"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// parseSealedKey decodes the X-CidVault-Key header value. A missing header
// is a 401 key_required; a present value that is not canonical standard
// Base64 of exactly 32 bytes is a 400 invalid_key.
func parseSealedKey(header http.Header) (key []byte, code string, status int) {
	raw := header.Get(sealedKeyHeader)
	if raw == "" {
		return nil, "key_required", http.StatusUnauthorized
	}
	key, err := base64DecodeStrict(raw)
	if err != nil || len(key) != sealedKeySize {
		return nil, "invalid_key", http.StatusBadRequest
	}
	return key, "", 0
}

func requireSealedKey(w http.ResponseWriter, r *http.Request) []byte {
	key, code, status := parseSealedKey(r.Header)
	if code != "" {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "CidVaultKey")
		}
		writeError(w, status, code)
		return nil
	}
	return key
}

// postSealedObject handles POST /v1/sealed-objects.
func (s *sealedStore) postSealedObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	key := requireSealedKey(w, r)
	if key == nil {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	// Read at most one byte past the limit so an oversized request is
	// detected before any state is produced.
	plaintext, err := io.ReadAll(io.LimitReader(r.Body, MaxObjectSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(plaintext) > MaxObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		// Already validated as a 32-byte key; nothing valid the caller
		// can send reaches this branch.
		writeError(w, http.StatusBadRequest, "invalid_key")
		return
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key")
		return
	}
	nonce := make([]byte, sealedNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	aad := sealedAAD(len(plaintext))
	// Seal appends the ciphertext and tag to dst and never reads the
	// plaintext again afterwards; the local copy is dropped on return.
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	cid := sealedCID(aad, nonce, ciphertext)
	created := s.insert(cid, &sealedObject{
		size:       len(plaintext),
		nonce:      nonce,
		ciphertext: ciphertext,
	})

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, sealedResponse{
		CID:       cid,
		Size:      len(plaintext),
		Algorithm: sealedAlgorithm,
	})
}

// resolveSealedObject validates the path identifier and fetches the stored
// envelope, writing the appropriate error response on failure.
func (s *sealedStore) resolveSealedObject(w http.ResponseWriter, r *http.Request) *sealedObject {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return nil
	}
	obj := s.snapshot(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "sealed_object_not_found")
		return nil
	}
	return obj
}

// sealedObjectByID dispatches GET and DELETE on
// /v1/sealed-objects/{cid}.
func (s *sealedStore) sealedObjectByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getSealedObject(w, r)
	case http.MethodDelete:
		s.deleteSealedObject(w, r)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// getSealedObject handles GET /v1/sealed-objects/{cid}.
func (s *sealedStore) getSealedObject(w http.ResponseWriter, r *http.Request) {
	key := requireSealedKey(w, r)
	if key == nil {
		return
	}
	obj := s.resolveSealedObject(w, r)
	if obj == nil {
		return
	}
	plaintext, ok := openSealed(key, obj)
	if !ok {
		writeError(w, http.StatusForbidden, "access_denied")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(plaintext)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(plaintext)
}

// getSealedEnvelope handles GET /v1/sealed-objects/{cid}/envelope. It needs
// no key: the disclosed data commits to its own CID and stays opaque.
func (s *sealedStore) getSealedEnvelope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	obj := s.resolveSealedObject(w, r)
	if obj == nil {
		return
	}
	cid := sealedCID(sealedAAD(obj.size), obj.nonce, obj.ciphertext)
	w.Header().Set("Content-Type", "application/vnd.cidvault.sealed+json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sealedEnvelope{
		Version:    1,
		Algorithm:  sealedAlgorithm,
		CID:        cid,
		Size:       obj.size,
		Nonce:      base64Encode(obj.nonce),
		Ciphertext: base64Encode(obj.ciphertext),
	})
}

// deleteSealedObject handles DELETE /v1/sealed-objects/{cid}.
func (s *sealedStore) deleteSealedObject(w http.ResponseWriter, r *http.Request) {
	key := requireSealedKey(w, r)
	if key == nil {
		return
	}
	obj := s.resolveSealedObject(w, r)
	if obj == nil {
		return
	}
	// The envelope must authenticate with the presented key before it may
	// be removed: knowing only the CID is not authority to delete.
	if _, ok := openSealed(key, obj); !ok {
		writeError(w, http.StatusForbidden, "access_denied")
		return
	}
	if !s.remove(r.PathValue("cid")) {
		// A concurrent caller holding the key deleted it first.
		writeError(w, http.StatusNotFound, "sealed_object_not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// openSealed decrypts an envelope snapshot, reporting false when the key or
// the authenticated data do not verify.
func openSealed(key []byte, obj *sealedObject) (plaintext []byte, ok bool) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false
	}
	aad := sealedAAD(obj.size)
	plaintext, err = gcm.Open(nil, obj.nonce, obj.ciphertext, aad)
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// base64Encode renders src as canonical RFC 4648 standard Base64.
func base64Encode(src []byte) string {
	return base64.StdEncoding.EncodeToString(src)
}

// base64DecodeStrict accepts only canonical standard Base64 (required
// padding, no whitespace) and rejects non-zero trailing padding bits:
// decoding and re-encoding must reproduce the input exactly.
func base64DecodeStrict(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if base64.StdEncoding.EncodeToString(raw) != s {
		return nil, fmt.Errorf("non-canonical base64")
	}
	return raw, nil
}
