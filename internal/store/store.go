// Package store implements the in-process content-addressed object store.
//
// Objects are split into fixed-size chunks. Each chunk is identified by
// sha256(chunk bytes), and an object's root identifier is the sha256 of a
// deterministic manifest encoding the object's total length and its ordered
// chunk identifiers.
//
// Everything lives in memory: objects are not retained across restarts, and
// there is intentionally no deletion, pinning or remote routing.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sync"
)

const (
	// ChunkSize is the fixed size in bytes of every chunk except the last.
	ChunkSize = 1 << 20 // 1048576

	// MaxObjectSize is the largest accepted object body in bytes (64 MiB).
	MaxObjectSize = 64 << 20 // 67108864
)

// CIDPrefix is the only supported content-identifier scheme.
const CIDPrefix = "sha256:"

// Object is the stored metadata for one object: its root CID, total length
// and ordered chunk CIDs.
type Object struct {
	Root   string
	Size   int64
	Chunks []string
}

type record struct {
	size   int64
	chunks []string
}

// Store is a goroutine-safe, in-process object store. Its zero value is not
// usable; create one with New.
type Store struct {
	mu    sync.RWMutex
	blobs map[string][]byte // chunk CID -> chunk bytes
	objs  map[string]record // root CID -> object metadata
}

// New returns an empty store.
func New() *Store {
	return &Store{
		blobs: make(map[string][]byte),
		objs:  make(map[string]record),
	}
}

func cidFor(sum [sha256.Size]byte) string {
	return CIDPrefix + hex.EncodeToString(sum[:])
}

// cloneChunks returns a private copy of chunks that is never nil, so empty
// lists serialize as [] rather than null.
func cloneChunks(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// RootCID computes the root identifier for an object: the sha256 of a
// deterministic binary manifest containing the total length followed by the
// ordered chunk CIDs. Each CID is length-prefixed (uint16, big-endian), so
// the framing is unambiguous; counts and length are uint64 big-endian.
func RootCID(size int64, chunks []string) string {
	b := make([]byte, 0, 16)
	b = binary.BigEndian.AppendUint64(b, uint64(size))
	b = binary.BigEndian.AppendUint64(b, uint64(len(chunks)))
	for _, c := range chunks {
		b = binary.BigEndian.AppendUint16(b, uint16(len(c)))
		b = append(b, c...)
	}
	return cidFor(sha256.Sum256(b))
}

// Put ingests one object body. It splits and hashes the body outside the
// store lock, then publishes the object atomically: the map mutation under
// the write lock is the only visibility point, so concurrent readers either
// see the whole object or nothing. Re-uploading identical bytes returns the
// same Object with created false.
func (s *Store) Put(data []byte) (Object, bool) {
	size := int64(len(data))

	// Chunk and hash without holding the lock. Stage private copies of each
	// chunk so the (large) request buffer is not pinned by stored slices.
	nChunks := int((size + ChunkSize - 1) / ChunkSize)
	chunks := make([]string, 0, nChunks)
	staged := make(map[string][]byte, nChunks)
	for remaining := data; len(remaining) > 0; {
		n := min(ChunkSize, len(remaining))
		part := remaining[:n]
		cid := cidFor(sha256.Sum256(part))
		chunks = append(chunks, cid)
		if _, ok := staged[cid]; !ok {
			blob := make([]byte, n)
			copy(blob, part)
			staged[cid] = blob
		}
		remaining = remaining[n:]
	}

	root := RootCID(size, chunks)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objs[root]; ok {
		// Identical object uploaded concurrently or repeated: reuse it, and
		// report it as not newly created.
		return Object{Root: root, Size: size, Chunks: cloneChunks(chunks)}, false
	}
	for cid, blob := range staged {
		if _, ok := s.blobs[cid]; !ok {
			s.blobs[cid] = blob
		}
	}
	s.objs[root] = record{size: size, chunks: cloneChunks(chunks)}
	return Object{Root: root, Size: size, Chunks: chunks}, true
}

// Stat returns the stored metadata for root, without its bytes.
func (s *Store) Stat(root string) (Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.objs[root]
	if !ok {
		return Object{}, false
	}
	return Object{Root: root, Size: rec.size, Chunks: cloneChunks(rec.chunks)}, true
}

// Get returns the object metadata and its chunks in reconstruction order.
// Concatenating the chunks reproduces the original body byte for byte.
func (s *Store) Get(root string) (Object, [][]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.objs[root]
	if !ok {
		return Object{}, nil, false
	}
	chunks := make([][]byte, len(rec.chunks))
	for i, cid := range rec.chunks {
		chunks[i] = s.blobs[cid]
	}
	obj := Object{Root: root, Size: rec.size, Chunks: cloneChunks(rec.chunks)}
	return obj, chunks, true
}
