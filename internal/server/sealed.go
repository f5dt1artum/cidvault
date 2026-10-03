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

// sealedAlgorithm names the encryption scheme for sealed objects.
const sealedAlgorithm = "AES-256-GCM"

// sealedKeyHeader carries the caller-held key on sealed-object requests.
const sealedKeyHeader = "X-CidVault-Key"

// sealedEnvelopeType is the media type of the keyless envelope view.
const sealedEnvelopeType = "application/vnd.cidvault.sealed+json"

// sealedObject is one encrypted envelope held in process memory. Only the
// nonce and the authenticated ciphertext are retained; the key and the
// plaintext never outlive the request that produced them.
type sealedObject struct {
	cid        string
	size       int    // plaintext length in bytes
	nonce      []byte // 12 bytes, generated per upload
	ciphertext []byte // GCM ciphertext with the authentication tag appended
}

// sealedStore keeps sealed envelopes isolated from the plain object store:
// they take no part in pins, garbage collection, bundles, statistics or the
// audit log. Envelopes are immutable once inserted; inserts and deletes
// happen under the write lock so readers observe either a complete envelope
// or none at all.
type sealedStore struct {
	mu      sync.RWMutex
	objects map[string]*sealedObject
}

func newSealedStore() *sealedStore {
	return &sealedStore{objects: make(map[string]*sealedObject)}
}

// sealedAAD returns the associated data bound into every seal and open.
func sealedAAD(size int) []byte {
	return []byte(fmt.Sprintf("cidvault-sealed-v1\nsize:%d\n", size))
}

// sealedCID derives the content identifier from the associated data, nonce
// and tag-carrying ciphertext, in the same "sha256:<hex>" shape as plain
// object identifiers.
func sealedCID(aad, nonce, ciphertext []byte) string {
	h := sha256.New()
	h.Write(aad)
	h.Write(nonce)
	h.Write(ciphertext)
	return cidPrefix + hex.EncodeToString(h.Sum(nil))
}

// zero overwrites b so key and plaintext copies do not linger in memory.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// requireSealedKey extracts and validates the caller-held key, writing the
// appropriate error response on failure. A missing header yields 401 with a
// WWW-Authenticate challenge; a malformed or wrong-length key yields 400.
func requireSealedKey(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	header := r.Header.Get(sealedKeyHeader)
	if header == "" {
		w.Header().Set("WWW-Authenticate", "CidVaultKey")
		writeError(w, http.StatusUnauthorized, "key_required")
		return nil, false
	}
	key, err := base64.StdEncoding.DecodeString(header)
	if err != nil || len(key) != 32 {
		writeError(w, http.StatusBadRequest, "invalid_key")
		return nil, false
	}
	return key, true
}

// openSealed decrypts the envelope with key, reporting whether
// authentication succeeded. The plaintext may be empty but is never nil on
// success.
func openSealed(rec *sealedObject, key []byte) ([]byte, bool) {
	gcm, err := newSealedGCM(key)
	if err != nil {
		return nil, false
	}
	plain, err := gcm.Open(nil, rec.nonce, rec.ciphertext, sealedAAD(rec.size))
	if err != nil {
		return nil, false
	}
	return plain, true
}

func newSealedGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *sealedStore) put(rec *sealedObject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[rec.cid] = rec
}

func (s *sealedStore) get(cid string) *sealedObject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objects[cid]
}

// remove deletes the envelope for cid, reporting whether it was present.
// Exactly one of several concurrent deleters observes true.
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

// sealedEnvelopeResponse is the keyless JSON view of a sealed object.
type sealedEnvelopeResponse struct {
	Version    int    `json:"version"`
	Algorithm  string `json:"algorithm"`
	CID        string `json:"cid"`
	Size       int    `json:"size"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// postSealedObject handles POST /v1/sealed-objects.
func (s *sealedStore) postSealedObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	key, ok := requireSealedKey(w, r)
	if !ok {
		return
	}
	defer zero(key)
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxObjectSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(body) > MaxObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}

	gcm, err := newSealedGCM(key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key")
		return
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	aad := sealedAAD(len(body))
	ciphertext := gcm.Seal(nil, nonce, body, aad)
	zero(body)

	rec := &sealedObject{
		cid:        sealedCID(aad, nonce, ciphertext),
		size:       len(body),
		nonce:      nonce,
		ciphertext: ciphertext,
	}
	s.put(rec)
	writeJSON(w, http.StatusCreated, sealedResponse{
		CID:       rec.cid,
		Size:      rec.size,
		Algorithm: sealedAlgorithm,
	})
}

// lookupSealed validates the path identifier and resolves the envelope,
// writing the appropriate error response on failure.
func (s *sealedStore) lookupSealed(w http.ResponseWriter, r *http.Request) *sealedObject {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return nil
	}
	rec := s.get(cid)
	if rec == nil {
		writeError(w, http.StatusNotFound, "sealed_object_not_found")
		return nil
	}
	return rec
}

// sealedObjectByID handles GET and DELETE /v1/sealed-objects/{cid}.
func (s *sealedStore) sealedObjectByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	rec := s.lookupSealed(w, r)
	if rec == nil {
		return
	}
	key, ok := requireSealedKey(w, r)
	if !ok {
		return
	}
	defer zero(key)
	plain, ok := openSealed(rec, key)
	if !ok {
		writeError(w, http.StatusForbidden, "access_denied")
		return
	}

	if r.Method == http.MethodDelete {
		zero(plain)
		if !s.remove(rec.cid) {
			writeError(w, http.StatusNotFound, "sealed_object_not_found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	defer zero(plain)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(rec.size))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(plain)
}

// getSealedEnvelope handles GET /v1/sealed-objects/{cid}/envelope. The
// envelope needs no key: it carries everything required to verify the CID
// and to decrypt offline, but never the plaintext.
func (s *sealedStore) getSealedEnvelope(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	rec := s.lookupSealed(w, r)
	if rec == nil {
		return
	}
	w.Header().Set("Content-Type", sealedEnvelopeType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sealedEnvelopeResponse{
		Version:    1,
		Algorithm:  sealedAlgorithm,
		CID:        rec.cid,
		Size:       rec.size,
		Nonce:      base64.StdEncoding.EncodeToString(rec.nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(rec.ciphertext),
	})
}
