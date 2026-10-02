package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
)

func chunkCID(b []byte) string {
	sum := sha256.Sum256(b)
	return CIDPrefix + hex.EncodeToString(sum[:])
}

func TestEmptyObjectHasNoChunksAndGoldenRoot(t *testing.T) {
	st := New()
	obj, created := st.Put(nil)
	if !created {
		t.Fatalf("first put: created = false, want true")
	}
	if obj.Size != 0 {
		t.Fatalf("size = %d, want 0", obj.Size)
	}
	if len(obj.Chunks) != 0 {
		t.Fatalf("empty body must have no data chunks, got %d", len(obj.Chunks))
	}
	// Manifest: uint64 size 0 + uint64 count 0 = 16 zero bytes.
	wantRoot := "sha256:374708fff7719dd5979ec875d56cd2286f6d3cf7ec317a3b25632aab28ec37bb"
	if obj.Root != wantRoot {
		t.Fatalf("root = %s, want %s", obj.Root, wantRoot)
	}
	if got := RootCID(0, []string{}); got != wantRoot {
		t.Fatalf("RootCID(0,[]) = %s, want %s", got, wantRoot)
	}
}

func TestChunkingAtBoundary(t *testing.T) {
	st := New()

	// Exactly one chunk: one full-length chunk, no empty tail.
	one := bytes.Repeat([]byte{'a'}, ChunkSize)
	obj, _ := st.Put(one)
	if len(obj.Chunks) != 1 || len(obj.Chunks[0]) == 0 {
		t.Fatalf("one-chunk object: chunks = %v", obj.Chunks)
	}
	if obj.Chunks[0] != chunkCID(one) {
		t.Fatalf("chunk CID is not sha256 of raw chunk bytes")
	}

	// One byte past: full chunk plus a 1-byte tail.
	two := bytes.Repeat([]byte{'b'}, ChunkSize+1)
	obj2, _ := st.Put(two)
	if len(obj2.Chunks) != 2 {
		t.Fatalf("two-chunk object: got %d chunks", len(obj2.Chunks))
	}
	if obj2.Chunks[0] != chunkCID(two[:ChunkSize]) || obj2.Chunks[1] != chunkCID(two[ChunkSize:]) {
		t.Fatalf("chunk CIDs do not match the raw chunk bytes in order")
	}
}

func TestDeterministicRootSensitiveToOrderCountAndContent(t *testing.T) {
	bodyA := bytes.Repeat([]byte{'A'}, ChunkSize)
	bodyB := make([]byte, ChunkSize+1)
	copy(bodyB, bodyA)
	bodyB[ChunkSize] = 'B'

	st := New()
	objA, _ := st.Put(bodyA)
	objB, _ := st.Put(bodyB)
	if objA.Root == objB.Root {
		t.Fatalf("adding a byte (chunk count change) must change root")
	}

	// Same chunk set reordered must change root.
	ci := chunkCID(bytes.Repeat([]byte{'i'}, 10))
	cj := chunkCID(bytes.Repeat([]byte{'j'}, 10))
	if RootCID(20, []string{ci, cj}) == RootCID(20, []string{cj, ci}) {
		t.Fatalf("root must depend on chunk order")
	}
	// Same order, different total length must change root.
	if RootCID(20, []string{ci, cj}) == RootCID(21, []string{ci, cj}) {
		t.Fatalf("root must depend on total length")
	}
}

func TestDuplicateUploadIsDeduped(t *testing.T) {
	st := New()
	body := []byte("the quick brown fox")
	first, created1 := st.Put(body)
	second, created2 := st.Put(append([]byte(nil), body...))
	if !created1 {
		t.Fatalf("first upload must be created")
	}
	if created2 {
		t.Fatalf("duplicate upload must not be created")
	}
	if first.Root != second.Root {
		t.Fatalf("identical bytes must yield identical root")
	}
}

func TestGetReassemblesByteForByte(t *testing.T) {
	st := New()
	body := bytes.Repeat([]byte("0123456789"), ChunkSize/5) // multiple chunks
	body = append(body, []byte("tail")...)                  // partial last chunk
	obj, _ := st.Put(body)

	got, chunks, ok := st.Get(obj.Root)
	if !ok {
		t.Fatalf("object missing after put")
	}
	if got.Size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", got.Size, len(body))
	}
	if !bytes.Equal(bytes.Join(chunks, nil), body) {
		t.Fatalf("reassembled body differs from original")
	}
	if _, ok := st.Stat("sha256:" + strings.Repeat("0", 64)); ok {
		t.Fatalf("Stat must miss unknown valid CID")
	}
}

func TestConcurrentUploadsCreateExactlyOnce(t *testing.T) {
	st := New()
	body := bytes.Repeat([]byte{'z'}, 2*ChunkSize+3)

	const n = 32
	var wg sync.WaitGroup
	results := make([]Object, n)
	created := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], created[i] = st.Put(append([]byte(nil), body...))
		}(i)
	}
	close(start)
	wg.Wait()

	creates := 0
	for i := 0; i < n; i++ {
		if results[i].Root != results[0].Root {
			t.Fatalf("concurrent puts diverged: %s vs %s", results[i].Root, results[0].Root)
		}
		if created[i] {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("created=true for %d of %d concurrent uploads, want exactly 1", creates, n)
	}
}
