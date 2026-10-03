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
	"strconv"
)

// bundleMediaType is the only media type accepted and produced by the
// bundle endpoints.
const bundleMediaType = "application/vnd.cidvault.bundle+json"

// bundleVersion is the only bundle format version this server understands.
const bundleVersion = 1

// maxBundleSize bounds the wire size of an import body. A bundle carrying a
// maximum-size object is dominated by the Base64 expansion of its blocks;
// the slack covers JSON framing, identifiers and whitespace.
const maxBundleSize = 4*(MaxObjectSize/3+1) + 1<<20

// bundleRoot mirrors the manifest of the exported object.
type bundleRoot struct {
	CID       string   `json:"cid"`
	Size      int      `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
}

// bundleBlock carries one unique block with its raw bytes in standard
// RFC 4648 Base64.
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
	dataByCID := make(map[string][]byte, len(obj.cids))
	for i, c := range obj.cids {
		dataByCID[c] = obj.chunks[i]
	}
	cids := make([]string, 0, len(dataByCID))
	for c := range dataByCID {
		cids = append(cids, c)
	}
	sort.Strings(cids)
	blocks := make([]bundleBlock, 0, len(cids))
	for _, c := range cids {
		blocks = append(blocks, bundleBlock{
			CID:  c,
			Data: base64.StdEncoding.EncodeToString(dataByCID[c]),
		})
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

// bundleRootJSON is the strictly decoded root section of an import body.
// Pointer fields distinguish a missing key from a zero value; numbers stay
// raw so quoted strings are rejected as type errors.
type bundleRootJSON struct {
	CID       *string          `json:"cid"`
	Size      *json.RawMessage `json:"size"`
	ChunkSize *json.RawMessage `json:"chunkSize"`
	Chunks    *[]string        `json:"chunks"`
}

// bundleBlockJSON is one strictly decoded block entry of an import body.
type bundleBlockJSON struct {
	CID  *string `json:"cid"`
	Data *string `json:"data"`
}

// bundleJSON is the strictly decoded import body.
type bundleJSON struct {
	Version *json.RawMessage   `json:"version"`
	Root    *bundleRootJSON    `json:"root"`
	Blocks  *[]bundleBlockJSON `json:"blocks"`
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
	obj, code, status := parseBundle(body)
	if code != "" {
		writeError(w, status, code)
		return
	}
	stored, created := s.put(obj)
	respStatus := http.StatusOK
	if created {
		respStatus = http.StatusCreated
	}
	writeJSON(w, respStatus, objectResponse{
		CID:       stored.cid,
		Size:      stored.size,
		ChunkSize: ChunkSize,
		Chunks:    stored.cids,
		Created:   created,
	})
}

// bundleErrors carries the error codes a bundle flavour reports. missing is
// empty for full bundles, where a block absent from the bundle is an
// integrity failure; delta bundles set it to the code reported when a block
// is neither in the bundle nor in the local store.
type bundleErrors struct {
	invalid     string
	unsupported string
	missing     string
}

// fullBundleErrors are the error codes of the complete single-object bundle.
var fullBundleErrors = bundleErrors{invalid: "invalid_bundle", unsupported: "unsupported_bundle"}

// parseBundle fully validates a complete bundle body; see parseBundleBody.
func parseBundle(body []byte) (obj *object, code string, status int) {
	return parseBundleBody(body, fullBundleErrors, nil)
}

// parseBundleBody fully validates a bundle body and, on success, returns the
// object ready to store. On failure it returns the error code and HTTP
// status to report; the store is never touched on the failure path. Blocks
// the manifest references but the bundle does not carry are obtained from
// resolve; a nil resolve means the bundle must be complete on its own.
func parseBundleBody(body []byte, codes bundleErrors, resolve func(cid string) []byte) (obj *object, code string, status int) {
	invalid := func() (*object, string, int) {
		return nil, codes.invalid, http.StatusBadRequest
	}
	integrity := func() (*object, string, int) {
		return nil, "integrity_check_failed", http.StatusUnprocessableEntity
	}

	if !wellFormedJSON(body) {
		return invalid()
	}
	var doc bundleJSON
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return invalid()
	}
	if doc.Version == nil || doc.Root == nil || doc.Blocks == nil ||
		doc.Root.CID == nil || doc.Root.Size == nil || doc.Root.ChunkSize == nil ||
		doc.Root.Chunks == nil {
		return invalid()
	}
	for _, b := range *doc.Blocks {
		if b.CID == nil || b.Data == nil {
			return invalid()
		}
	}

	// Numeric fields must be integers. An integer outside int64 range is a
	// value problem, not a type problem: it cannot match a supported
	// version, chunk size or size limit.
	version, err := parseInt(*doc.Version)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, codes.unsupported, http.StatusUnprocessableEntity
		}
		return invalid()
	}
	chunkSize, err := parseInt(*doc.Root.ChunkSize)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, codes.unsupported, http.StatusUnprocessableEntity
		}
		return invalid()
	}
	size, err := parseInt(*doc.Root.Size)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, "payload_too_large", http.StatusRequestEntityTooLarge
		}
		return invalid()
	}

	if !validCID(*doc.Root.CID) {
		return invalid()
	}
	for _, c := range *doc.Root.Chunks {
		if !validCID(c) {
			return invalid()
		}
	}
	blockData := make(map[string][]byte, len(*doc.Blocks))
	for _, b := range *doc.Blocks {
		if !validCID(*b.CID) {
			return invalid()
		}
		data, err := base64.StdEncoding.DecodeString(*b.Data)
		if err != nil {
			return invalid()
		}
		blockData[*b.CID] = data
	}

	if version != bundleVersion || chunkSize != ChunkSize {
		return nil, codes.unsupported, http.StatusUnprocessableEntity
	}
	if size > MaxObjectSize {
		return nil, "payload_too_large", http.StatusRequestEntityTooLarge
	}
	if size < 0 {
		return integrity()
	}

	// The bundle must not repeat a block or carry a block the manifest does
	// not reference. Blocks the manifest references but the bundle omits are
	// resolved below. Duplicates are tracked separately because blockData
	// collapses them while decoding.
	referenced := make(map[string]bool, len(*doc.Root.Chunks))
	for _, c := range *doc.Root.Chunks {
		referenced[c] = true
	}
	seen := make(map[string]bool, len(*doc.Blocks))
	for _, b := range *doc.Blocks {
		if seen[*b.CID] {
			return integrity()
		}
		seen[*b.CID] = true
		if !referenced[*b.CID] {
			return integrity()
		}
	}

	// Every block identifier must be the digest of its decoded bytes.
	for cid, data := range blockData {
		if chunkCID(data) != cid {
			return integrity()
		}
	}

	// A complete bundle carries every referenced block; a delta bundle
	// resolves the rest against the local store and reports a miss as such.
	for c := range referenced {
		if _, ok := blockData[c]; ok {
			continue
		}
		if resolve == nil {
			return integrity()
		}
		data := resolve(c)
		if data == nil {
			return nil, codes.missing, http.StatusUnprocessableEntity
		}
		blockData[c] = data
	}

	// Rebuild the body in manifest order under the fixed chunking rules:
	// every chunk but the last is exactly ChunkSize, the last is 1..ChunkSize,
	// and an empty object carries no chunks at all.
	chunks := *doc.Root.Chunks
	if size == 0 && len(chunks) != 0 {
		return integrity()
	}
	if size > 0 && len(chunks) == 0 {
		return integrity()
	}
	total := 0
	for i, c := range chunks {
		n := len(blockData[c])
		if i < len(chunks)-1 {
			if n != ChunkSize {
				return integrity()
			}
		} else if n < 1 || n > ChunkSize {
			return integrity()
		}
		total += n
	}
	if total > MaxObjectSize {
		return nil, "payload_too_large", http.StatusRequestEntityTooLarge
	}
	if total != size {
		return integrity()
	}
	if rootCID(size, chunks) != *doc.Root.CID {
		return integrity()
	}

	obj = &object{
		cid:    *doc.Root.CID,
		size:   size,
		chunks: make([][]byte, 0, len(chunks)),
		cids:   append([]string{}, chunks...),
	}
	for _, c := range chunks {
		obj.chunks = append(obj.chunks, blockData[c])
	}
	return obj, "", 0
}

// wellFormedJSON reports whether data is a single well-formed JSON value
// with no duplicate object keys and nothing but whitespace after it.
func wellFormedJSON(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() bool
	walk = func() bool {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk() {
					return false
				}
			}
			_, err := dec.Token()
			return err == nil
		case '[':
			for dec.More() {
				if !walk() {
					return false
				}
			}
			_, err := dec.Token()
			return err == nil
		}
		return false
	}
	if !walk() {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}

// parseInt converts a raw JSON value to an int, rejecting strings,
// non-numeric literals and fractional values as syntax errors.
func parseInt(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || raw[0] == '"' {
		return 0, strconv.ErrSyntax
	}
	v, err := strconv.ParseInt(string(raw), 10, 0)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}
