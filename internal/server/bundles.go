package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
)

// bundleMediaType is the only media type accepted and produced by the
// single-object bundle endpoints.
const bundleMediaType = "application/vnd.cidvault.bundle+json"

// bundleVersion is the only bundle layout version this service understands.
const bundleVersion = 1

// maxBundleSize bounds the JSON body of POST /v1/bundles: the Base64
// expansion of a maximum-size object plus generous framing slack.
const maxBundleSize = (MaxObjectSize+2)/3*4 + 1<<20

// bundleRoot is the manifest section of an exported bundle.
type bundleRoot struct {
	CID       string   `json:"cid"`
	Size      int      `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
}

// bundleBlock is one unique block in an exported bundle. Data carries the
// raw block bytes encoded as RFC 4648 standard Base64.
type bundleBlock struct {
	CID  string `json:"cid"`
	Data string `json:"data"`
}

// bundleDocument is the JSON body of GET /v1/bundles/{cid}.
type bundleDocument struct {
	Version int           `json:"version"`
	Root    bundleRoot    `json:"root"`
	Blocks  []bundleBlock `json:"blocks"`
}

// bundleRootInput is the strictly decoded root section of a bundle.
type bundleRootInput struct {
	CID       *string  `json:"cid"`
	Size      *int64   `json:"size"`
	ChunkSize *int64   `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
}

// bundleInput mirrors bundleDocument for strict decoding: pointer fields
// distinguish a missing (or null) field from a zero value.
type bundleInput struct {
	Version *int64           `json:"version"`
	Root    *bundleRootInput `json:"root"`
	Blocks  []struct {
		CID  *string `json:"cid"`
		Data *string `json:"data"`
	} `json:"blocks"`
}

// errInvalidBundle marks any structurally invalid bundle document; the
// handler maps it to 400 invalid_bundle.
var errInvalidBundle = errors.New("invalid bundle")

// getBundle handles GET /v1/bundles/{cid}.
func (s *store) getBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	obj := s.lookupObject(w, r)
	if obj == nil {
		return
	}
	unique := make(map[string][]byte, len(obj.cids))
	for i, c := range obj.cids {
		if _, ok := unique[c]; !ok {
			unique[c] = obj.chunks[i]
		}
	}
	cids := make([]string, 0, len(unique))
	for c := range unique {
		cids = append(cids, c)
	}
	sort.Strings(cids)
	blocks := make([]bundleBlock, 0, len(cids))
	for _, c := range cids {
		blocks = append(blocks, bundleBlock{CID: c, Data: base64.StdEncoding.EncodeToString(unique[c])})
	}
	w.Header().Set("Content-Type", bundleMediaType)
	_ = json.NewEncoder(w).Encode(bundleDocument{
		Version: bundleVersion,
		Root: bundleRoot{
			CID:       obj.cid,
			Size:      obj.size,
			ChunkSize: ChunkSize,
			Chunks:    obj.cids,
		},
		Blocks: blocks,
	})
}

// postBundle handles POST /v1/bundles.
func (s *store) postBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != bundleMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBundleSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bundle")
		return
	}
	if len(body) > maxBundleSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	doc, err := parseBundle(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_bundle")
		return
	}
	root := doc.Root
	if *root.Size < 0 {
		writeError(w, http.StatusBadRequest, "invalid_bundle")
		return
	}
	if !validCID(*root.CID) {
		writeError(w, http.StatusBadRequest, "invalid_bundle")
		return
	}
	for _, c := range root.Chunks {
		if !validCID(c) {
			writeError(w, http.StatusBadRequest, "invalid_bundle")
			return
		}
	}
	blocks := make(map[string][]byte, len(doc.Blocks))
	duplicateBlock := false
	for i := range doc.Blocks {
		cid, encoded := *doc.Blocks[i].CID, *doc.Blocks[i].Data
		if !validCID(cid) {
			writeError(w, http.StatusBadRequest, "invalid_bundle")
			return
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_bundle")
			return
		}
		if _, exists := blocks[cid]; exists {
			duplicateBlock = true
		}
		blocks[cid] = data
	}
	if *doc.Version != bundleVersion || *root.ChunkSize != ChunkSize {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_bundle")
		return
	}
	if *root.Size > MaxObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	if err := verifyBundleIntegrity(root, blocks, duplicateBlock); err != nil {
		if errors.Is(err, errBundleTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		} else {
			writeError(w, http.StatusUnprocessableEntity, "integrity_check_failed")
		}
		return
	}
	obj := &object{cid: *root.CID, size: int(*root.Size), cids: root.Chunks}
	obj.chunks = make([][]byte, len(root.Chunks))
	for i, c := range root.Chunks {
		obj.chunks[i] = blocks[c]
	}
	stored, created := s.put(obj)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, objectResponse{
		CID:       stored.cid,
		Size:      stored.size,
		ChunkSize: ChunkSize,
		Chunks:    stored.cids,
		Created:   created,
	})
}

// errBundleTooLarge marks a bundle whose reconstructed object exceeds the
// size limit; every other integrity failure uses errInvalidBundle's
// integrity mapping in the handler.
var errBundleTooLarge = errors.New("rebuilt object too large")

// verifyBundleIntegrity checks the content-addressed invariants of a
// structurally valid bundle: block digests, the exact block set, the fixed
// chunking layout, the rebuilt length and the root identifier.
func verifyBundleIntegrity(root *bundleRootInput, blocks map[string][]byte, duplicateBlock bool) error {
	for cid, data := range blocks {
		if chunkCID(data) != cid {
			return errInvalidBundle
		}
	}
	referenced := make(map[string]bool, len(root.Chunks))
	for _, c := range root.Chunks {
		referenced[c] = true
	}
	if duplicateBlock || len(blocks) != len(referenced) {
		return errInvalidBundle
	}
	for c := range referenced {
		if _, ok := blocks[c]; !ok {
			return errInvalidBundle
		}
	}
	total := 0
	for i, c := range root.Chunks {
		n := len(blocks[c])
		if i+1 < len(root.Chunks) && n != ChunkSize {
			return errInvalidBundle
		}
		total += n
		if total > MaxObjectSize {
			return errBundleTooLarge
		}
	}
	if n := len(root.Chunks); n > 0 {
		if last := len(blocks[root.Chunks[n-1]]); last == 0 || last > ChunkSize {
			return errInvalidBundle
		}
	}
	if total != int(*root.Size) {
		return errInvalidBundle
	}
	if rootCID(int(*root.Size), root.Chunks) != *root.CID {
		return errInvalidBundle
	}
	return nil
}

// parseBundle strictly decodes a bundle document: malformed JSON, trailing
// content, missing, unknown or repeated fields and wrong field types all
// fail. Value-level rules (identifier formats, sizes, digests) are checked
// by the caller.
func parseBundle(body []byte) (*bundleInput, error) {
	if err := rejectDuplicateKeys(body); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var doc bundleInput
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errInvalidBundle
	}
	if doc.Version == nil || doc.Root == nil || doc.Blocks == nil {
		return nil, errInvalidBundle
	}
	root := doc.Root
	if root.CID == nil || root.Size == nil || root.ChunkSize == nil || root.Chunks == nil {
		return nil, errInvalidBundle
	}
	for i := range doc.Blocks {
		if doc.Blocks[i].CID == nil || doc.Blocks[i].Data == nil {
			return nil, errInvalidBundle
		}
	}
	return &doc, nil
}

// rejectDuplicateKeys scans a JSON document and fails when any object
// repeats a key, which encoding/json would otherwise silently accept.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		object  bool
		wantKey bool
		keys    map[string]bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, wantKey: true, keys: make(map[string]bool)})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) == 0 {
					return errInvalidBundle
				}
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].object {
					stack[n-1].wantKey = true
				}
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object {
				top := stack[n-1]
				if top.wantKey {
					if top.keys[t] {
						return errInvalidBundle
					}
					top.keys[t] = true
					top.wantKey = false
				} else {
					top.wantKey = true
				}
			}
		default: // number, boolean or null value
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].wantKey = true
			}
		}
	}
}
