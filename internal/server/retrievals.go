package server

import (
	"io"
	"mime"
	"net/http"
	"time"
)

// retrievalTimeout bounds every attempt to fetch one provider address.
const retrievalTimeout = 30 * time.Second

// retrievalMediaType is the only media type a provider response may carry.
const retrievalMediaType = "application/octet-stream"

// retriever serves POST /v1/retrievals/{cid}: when an object is missing
// locally it fetches the original bytes from the providers published in the
// directory and stores them through the same atomic path as an upload.
type retriever struct {
	store     *store
	providers *providerDirectory
	client    *http.Client
}

func newRetriever(s *store, d *providerDirectory) *retriever {
	return &retriever{
		store:     s,
		providers: d,
		client: &http.Client{
			Timeout: retrievalTimeout,
			// Addresses are fetched verbatim and must never be followed: a
			// redirect response is just another failed address.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// retrievalResponse extends the upload response with the provider location
// actually used. The early "already present" response leaves the pointers
// nil, so both fields marshal as null.
type retrievalResponse struct {
	CID        string   `json:"cid"`
	Size       int      `json:"size"`
	ChunkSize  int      `json:"chunkSize"`
	Chunks     []string `json:"chunks"`
	Created    bool     `json:"created"`
	ProviderID *string  `json:"providerId"`
	Address    *string  `json:"address"`
}

// handleRetrieval handles POST /v1/retrievals/{cid}.
func (rt *retriever) handleRetrieval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}

	// An object present at acceptance is served without touching the
	// network, regardless of what the directory says.
	if obj := rt.store.get(cid); obj != nil {
		writeJSON(w, http.StatusOK, retrievalResponse{
			CID:       obj.cid,
			Size:      obj.size,
			ChunkSize: ChunkSize,
			Chunks:    obj.cids,
			Created:   false,
		})
		return
	}

	// Snapshot the still-valid providers at a single instant. The attempt
	// order is provider ID order followed by each record's address order;
	// later expiry or deletion never reshuffles this request.
	entries := rt.providers.list(cid, time.Now())
	if len(entries) == 0 {
		writeError(w, http.StatusNotFound, "no_provider")
		return
	}
	for _, entry := range entries {
		for _, address := range entry.addresses {
			body, ok := rt.fetch(address)
			if !ok {
				continue
			}
			obj := buildObject(body)
			if obj.cid != cid {
				continue
			}
			stored, created := rt.store.put(obj)
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			providerID := entry.providerID

			writeJSON(w, status, retrievalResponse{
				CID:        stored.cid,
				Size:       stored.size,
				ChunkSize:  ChunkSize,
				Chunks:     stored.cids,
				Created:    created,
				ProviderID: &providerID,
				Address:    &address,
			})
			return
		}
	}
	writeError(w, http.StatusBadGateway, "retrieval_failed")
}

// fetch GETs one provider address verbatim, carrying no caller credentials
// and accepting only a 200 application/octet-stream body within the object
// size limit. Any deviation reports ok == false and the caller moves on to
// the next address; nothing fetched here touches the store.
func (rt *retriever) fetch(address string) (body []byte, ok bool) {
	req, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, false
	}
	resp, err := rt.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != retrievalMediaType {
		return nil, false
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, MaxObjectSize+1))
	if err != nil || len(body) > MaxObjectSize {
		return nil, false
	}
	return body, true
}

// buildObject applies the ordinary upload rules to retrieved bytes: fixed
// chunking, per-chunk digests and the deterministic manifest root.
func buildObject(body []byte) *object {
	obj := &object{size: len(body), chunks: [][]byte{}, cids: []string{}}
	for start := 0; start < len(body); start += ChunkSize {
		end := start + ChunkSize
		if end > len(body) {
			end = len(body)
		}
		chunk := body[start:end]
		obj.chunks = append(obj.chunks, chunk)
		obj.cids = append(obj.cids, chunkCID(chunk))
	}
	obj.cid = rootCID(obj.size, obj.cids)
	return obj
}
