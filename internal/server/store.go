package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
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
}

func newStore() *store {
	return &store{objects: make(map[string]*object)}
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
