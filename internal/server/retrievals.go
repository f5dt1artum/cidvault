package server

import (
	"io"
	"mime"
	"net/http"
	"time"
)

// retrievalTimeout bounds a single address fetch, including the body read.
const retrievalTimeout = 30 * time.Second

// retrievalResponse is the JSON body of POST /v1/retrievals/{cid}. It
// extends the upload response with the provider and address that supplied
// the body; both are null when the object was already stored locally.
type retrievalResponse struct {
	CID        string   `json:"cid"`
	Size       int      `json:"size"`
	ChunkSize  int      `json:"chunkSize"`
	Chunks     []string `json:"chunks"`
	Created    bool     `json:"created"`
	ProviderID *string  `json:"providerId"`
	Address    *string  `json:"address"`
}

// retriever fetches missing objects from the addresses published in the
// provider directory.
type retriever struct {
	objects   *store
	providers *providerDirectory
	client    *http.Client
}

func newRetriever(objects *store, providers *providerDirectory) *retriever {
	return &retriever{
		objects:   objects,
		providers: providers,
		client: &http.Client{
			Timeout: retrievalTimeout,
			// Redirects are not followed: a 3xx response fails the address.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// postRetrieval handles POST /v1/retrievals/{cid}.
func (rv *retriever) postRetrieval(w http.ResponseWriter, r *http.Request) {
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
	// An object already stored at acceptance time is served locally; the
	// network is never touched.
	if obj := rv.objects.get(cid); obj != nil {
		rv.objects.audit.append(auditRecord{
			action: auditRetrievalLocal,
			result: "local",
			cid:    strPtr(obj.cid),
			bytes:  intPtr(obj.size),
		})
		writeJSON(w, http.StatusOK, retrievalResponse{
			CID:       obj.cid,
			Size:      obj.size,
			ChunkSize: ChunkSize,
			Chunks:    obj.cids,
			Created:   false,
		})
		return
	}
	// Snapshot the providers valid at acceptance time; records expiring or
	// deleted afterwards do not change this attempt's order.
	providers := rv.providers.list(cid, time.Now())
	if len(providers) == 0 {
		writeError(w, http.StatusNotFound, "no_provider")
		return
	}
	for _, p := range providers {
		for _, address := range p.addresses {
			body, ok := rv.fetch(address)
			if !ok {
				continue
			}
			obj := newObject(body)
			if obj.cid != cid {
				continue
			}
			providerID := p.providerID
			stored, created := rv.objects.put(obj, auditRetrievalFetch, &providerID)
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			addr := address
			writeJSON(w, status, retrievalResponse{
				CID:        stored.cid,
				Size:       stored.size,
				ChunkSize:  ChunkSize,
				Chunks:     stored.cids,
				Created:    created,
				ProviderID: &providerID,
				Address:    &addr,
			})
			return
		}
	}
	// Every address failed; nothing was stored, pinned or counted.
	writeError(w, http.StatusBadGateway, "retrieval_failed")
}

// fetch GETs one provider address exactly as published: no path is appended
// and no caller credentials are forwarded. It returns the raw body only when
// the response is a 200 application/octet-stream within the object size
// limit; every other outcome fails the address.
func (rv *retriever) fetch(address string) ([]byte, bool) {
	req, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, false
	}
	resp, err := rv.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxObjectSize+1))
	if err != nil || len(body) > MaxObjectSize {
		return nil, false
	}
	return body, true
}
