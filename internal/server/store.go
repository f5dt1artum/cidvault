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

// store is an in-process content-addressed object store. Objects are
// immutable once inserted; inserts happen under the write lock so readers
// never observe a partially written object.
type store struct {
	mu      sync.RWMutex
	objects map[string]*object
	pins    map[string]*pin
}

func newStore() *store {
	return &store{
		objects: make(map[string]*object),
		pins:    make(map[string]*pin),
	}
}

// pin is a retention record for one object. A nil expiresAt means the pin is
// retained indefinitely; otherwise it is valid only until expiresAt.
type pin struct {
	cid       string
	expiresAt *time.Time
}

// validAt reports whether the pin currently protects its object.
func (p *pin) validAt(now time.Time) bool {
	return p.expiresAt == nil || p.expiresAt.After(now)
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
// created == true for a given object.
func (s *store) put(obj *object) (stored *object, created bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.objects[obj.cid]; ok {
		return existing, false
	}
	s.objects[obj.cid] = obj
	return obj, true
}

// get returns the object for a root identifier, or nil when absent.
func (s *store) get(cid string) *object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objects[cid]
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

// pinInfo is an immutable view of one currently valid pin.
type pinInfo struct {
	cid       string
	expiresAt *time.Time
}

// upsertPin creates or replaces the pin for cid. ok is false when the object
// does not exist; updated is true only when an already valid pin was
// replaced. Expiration and object existence are decided together under the
// store lock, so a concurrent garbage collection cannot delete an object
// whose pin succeeds in the same acceptance order.
func (s *store) upsertPin(cid string, expiresAt *time.Time, now time.Time) (updated, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[cid]; !exists {
		return false, false
	}
	if existing, pinned := s.pins[cid]; pinned && existing.validAt(now) {
		updated = true
	}
	s.pins[cid] = &pin{cid: cid, expiresAt: expiresAt}
	return updated, true
}

// deletePin removes the pin for cid. It returns false when there is no
// currently valid pin, including pins that have already expired.
func (s *store) deletePin(cid string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, pinned := s.pins[cid]
	if !pinned || !existing.validAt(now) {
		return false
	}
	delete(s.pins, cid)
	return true
}

// listPins returns all currently valid pins ordered by cid.
func (s *store) listPins(now time.Time) []pinInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []pinInfo
	for _, p := range s.pins {
		if !p.validAt(now) {
			continue
		}
		out = append(out, pinInfo{cid: p.cid, expiresAt: p.expiresAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cid < out[j].cid })
	return out
}

// gcResult describes a garbage collection pass. The same values describe the
// preview when dryRun is true.
type gcResult struct {
	cids  []string
	bytes int
}

// collectGarbage identifies (and, unless dryRun, deletes) every object that
// has no valid pin at acceptance time. An expired pin counts as unpinned;
// permanent and unexpired pins protect their objects. Deletions happen under
// the write lock, so concurrent readers see either the whole object or no
// object, never partial bytes or a half-removed manifest.
func (s *store) collectGarbage(now time.Time, dryRun bool) gcResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res gcResult
	for cid, obj := range s.objects {
		if p, pinned := s.pins[cid]; pinned && p.validAt(now) {
			continue
		}
		res.cids = append(res.cids, cid)
		res.bytes += obj.size
	}
	sort.Strings(res.cids)

	if !dryRun {
		for _, cid := range res.cids {
			delete(s.objects, cid)
			// Remove any expired pin record alongside the deleted object;
			// valid pins never match the candidate set above.
			if p, pinned := s.pins[cid]; pinned && !p.validAt(now) {
				delete(s.pins, cid)
			}
		}
	}
	return res
}
