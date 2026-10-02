package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChunkSize is the fixed split size for object bodies. All chunks except the
// final one have exactly this many bytes.
const ChunkSize = 1048576

// MaxObjectSize is the largest accepted object body in bytes.
const MaxObjectSize = 67108864

// cidPrefix prefixes every content identifier.
const cidPrefix = "sha256:"

// object is a fully received object held in process memory.
type object struct {
	cid    string
	size   int
	chunks [][]byte // raw chunk bytes, in reconstruction order
	cids   []string // chunk identifiers, parallel to chunks
}

// pin records a retention guarantee for one object. A nil expiresAt means
// the object is retained permanently; otherwise the pin is in force until
// the given instant. Pins live in process memory only and are not preserved
// across restarts.
type pin struct {
	expiresAt *time.Time
}

// validPin reports whether p is in force at now. Expired pins are
// equivalent to no pin at all.
func validPin(p pin, now time.Time) bool {
	return p.expiresAt == nil || p.expiresAt.After(now)
}

// blockRef holds one unique stored block. refs counts the live objects that
// reference the block; a block repeated within one object still counts once.
type blockRef struct {
	data []byte
	refs int
}

// store is an in-process content-addressed object store. Objects are
// immutable once inserted; inserts and deletes happen under the write lock
// so readers never observe a partially written object. Blocks are stored
// once and shared by every object that references them; a block is dropped
// as soon as no live object references it.
type store struct {
	mu      sync.RWMutex
	objects map[string]*object
	blocks  map[string]*blockRef
	pins    map[string]pin
}

func newStore() *store {
	return &store{
		objects: make(map[string]*object),
		blocks:  make(map[string]*blockRef),
		pins:    make(map[string]pin),
	}
}

// chunkCID returns the identifier for one chunk's raw bytes.
func chunkCID(data []byte) string {
	sum := sha256.Sum256(data)
	return cidPrefix + hex.EncodeToString(sum[:])
}

// rootCID derives the object identifier from the deterministic manifest: the
// total object length followed by the ordered chunk identifiers, encoded as
// "size:<N>\n" then one "<cid>\n" line per chunk.
func rootCID(size int, cids []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "size:%d\n", size)
	for _, c := range cids {
		b.WriteString(c)
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return cidPrefix + hex.EncodeToString(sum[:])
}

// put stores the object, or returns the existing one when the same root
// identifier is already present. Exactly one concurrent caller observes
// created == true for a given object. Newly stored objects register their
// unique chunks in the shared block table; repeated chunks within the
// object and chunks shared with other objects are stored only once.
func (s *store) put(obj *object) (stored *object, created bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.objects[obj.cid]; ok {
		return existing, false
	}
	s.objects[obj.cid] = obj
	seen := make(map[string]bool, len(obj.cids))
	for i, c := range obj.cids {
		if b, ok := s.blocks[c]; ok {
			obj.chunks[i] = b.data // share the stored copy
			if !seen[c] {
				b.refs++
			}
		} else {
			s.blocks[c] = &blockRef{data: obj.chunks[i], refs: 1}
		}
		seen[c] = true
	}
	return obj, true
}

// get returns the object for a root identifier, or nil when absent.
func (s *store) get(cid string) *object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objects[cid]
}

// getBlock returns the raw bytes of a stored block, or nil when no live
// object references it.
func (s *store) getBlock(cid string) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.blocks[cid]; ok {
		return b.data
	}
	return nil
}

// storageStats is a consistent snapshot of the store's occupancy.
type storageStats struct {
	objects      int
	logicalBytes int
	blocks       int
	storedBytes  int
}

// stats snapshots object and deduplicated block occupancy under one lock
// hold, so the four counters always describe the same instant.
func (s *store) stats() storageStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := storageStats{objects: len(s.objects), blocks: len(s.blocks)}
	for _, obj := range s.objects {
		st.logicalBytes += obj.size
	}
	for _, b := range s.blocks {
		st.storedBytes += len(b.data)
	}
	return st
}

// setPin pins cid with the given expiration, replacing any previous pin. It
// reports ok == false when the object does not exist. created is true when
// no valid pin was in force at now. The check and the update are atomic
// with respect to garbage collection.
func (s *store) setPin(cid string, expiresAt *time.Time, now time.Time) (created, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[cid]; !exists {
		return false, false
	}
	existing, found := s.pins[cid]
	created = !found || !validPin(existing, now)
	s.pins[cid] = pin{expiresAt: expiresAt}
	return created, true
}

// removePin deletes the pin for cid, reporting whether a valid pin was in
// force at now. Expired pins are removed lazily and reported as absent.
func (s *store) removePin(cid string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, found := s.pins[cid]
	if !found || !validPin(p, now) {
		delete(s.pins, cid)
		return false
	}
	delete(s.pins, cid)
	return true
}

// pinEntry is one live pin in a listing.
type pinEntry struct {
	cid       string
	expiresAt *time.Time
}

// listPins returns the pins in force at now, sorted by cid.
func (s *store) listPins(now time.Time) []pinEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := []pinEntry{}
	for cid, p := range s.pins {
		if validPin(p, now) {
			entries = append(entries, pinEntry{cid: cid, expiresAt: p.expiresAt})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].cid < entries[j].cid })
	return entries
}

// collectGarbage removes every object with no valid pin in force at now, or
// only counts them when dryRun is true. It returns the affected cids in
// lexical order and the sum of their body sizes. The sweep is atomic with
// respect to pin updates and object reads.
func (s *store) collectGarbage(now time.Time, dryRun bool) (cids []string, totalBytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cids = []string{}
	for cid, obj := range s.objects {
		if p, pinned := s.pins[cid]; pinned && validPin(p, now) {
			continue
		}
		cids = append(cids, cid)
		totalBytes += obj.size
	}
	sort.Strings(cids)
	if !dryRun {
		for _, cid := range cids {
			obj := s.objects[cid]
			delete(s.objects, cid)
			s.releaseBlocks(obj)
		}
		for cid, p := range s.pins {
			if !validPin(p, now) {
				delete(s.pins, cid)
			}
		}
	}
	return cids, totalBytes
}

// releaseBlocks drops the object's block references, removing any block no
// other live object uses. The caller must hold the write lock.
func (s *store) releaseBlocks(obj *object) {
	seen := make(map[string]bool, len(obj.cids))
	for _, c := range obj.cids {
		if seen[c] {
			continue
		}
		seen[c] = true
		b, ok := s.blocks[c]
		if !ok {
			continue
		}
		b.refs--
		if b.refs == 0 {
			delete(s.blocks, c)
		}
	}
}

// validCID reports whether s has the "sha256:<64 lowercase hex>" shape.
func validCID(s string) bool {
	if len(s) != len(cidPrefix)+64 || !strings.HasPrefix(s, cidPrefix) {
		return false
	}
	for _, c := range s[len(cidPrefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
