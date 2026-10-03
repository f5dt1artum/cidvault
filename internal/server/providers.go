package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxProviderAddresses bounds the addresses one provider record may carry.
const maxProviderAddresses = 16

// maxProviderBodySize bounds the wire size of a provider publication body.
// The protocol itself only bounds the address count; the cap just keeps an
// unbounded request from exhausting memory.
const maxProviderBodySize = 1 << 20

// maxProviderTTL is the longest lifetime a provider record may announce.
const maxProviderTTL = 24 * time.Hour

// providerRecord is one provider's advertised access locations for a CID.
type providerRecord struct {
	addresses []string
	expiresAt time.Time
}

// providerDirectory is a process-local index of provider records keyed by
// CID and then provider ID. Publishing a record does not require the object
// to be stored locally; the directory takes no part in pinning, garbage
// collection or storage statistics, and records are not retained across
// restarts.
type providerDirectory struct {
	mu      sync.RWMutex
	records map[string]map[string]providerRecord
}

func newProviderDirectory() *providerDirectory {
	return &providerDirectory{records: make(map[string]map[string]providerRecord)}
}

// put publishes or replaces the record for (cid, providerID) and reports
// whether a valid record existed at now. A first publication or a
// replacement of an expired record reports created == true; exactly one
// concurrent first publisher observes it for a given key.
func (d *providerDirectory) put(cid, providerID string, addresses []string, expiresAt, now time.Time) (created bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	byProvider := d.records[cid]
	if byProvider == nil {
		byProvider = make(map[string]providerRecord)
		d.records[cid] = byProvider
	}
	existing, found := byProvider[providerID]
	created = !found || !existing.expiresAt.After(now)
	byProvider[providerID] = providerRecord{
		addresses: append([]string(nil), addresses...),
		expiresAt: expiresAt,
	}
	return created
}

// providerEntry is one live record in a point-in-time directory listing.
type providerEntry struct {
	providerID string
	addresses  []string
	expiresAt  time.Time
}

// list returns the records valid at now for cid, sorted by provider ID.
// The slice and its address arrays are copies, so callers keep a consistent
// snapshot even while other requests mutate the directory.
func (d *providerDirectory) list(cid string, now time.Time) []providerEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	entries := []providerEntry{}
	for id, rec := range d.records[cid] {
		if !rec.expiresAt.After(now) {
			continue
		}
		entries = append(entries, providerEntry{
			providerID: id,
			addresses:  append([]string(nil), rec.addresses...),
			expiresAt:  rec.expiresAt,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].providerID < entries[j].providerID })
	return entries
}

// remove deletes the record for (cid, providerID) when one is in force at
// now, reporting whether such a record existed. Expired records are treated
// as absent.
func (d *providerDirectory) remove(cid, providerID string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	byProvider := d.records[cid]
	rec, found := byProvider[providerID]
	if !found || !rec.expiresAt.After(now) {
		return false
	}
	delete(byProvider, providerID)
	if len(byProvider) == 0 {
		delete(d.records, cid)
	}
	return true
}

// providerPutRequest is the strictly decoded body of
// PUT /v1/providers/{cid}/{providerId}. Raw pointer fields distinguish a
// missing key (nil) from an explicit null and keep type errors distinct
// from value errors.
type providerPutRequest struct {
	Addresses *json.RawMessage `json:"addresses"`
	ExpiresAt *json.RawMessage `json:"expiresAt"`
}

// providerRecordResponse is the JSON representation of one provider record.
type providerRecordResponse struct {
	ProviderID string    `json:"providerId"`
	Addresses  []string  `json:"addresses"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// providerListResponse is the JSON body of GET /v1/providers/{cid}.
type providerListResponse struct {
	CID       string                   `json:"cid"`
	Providers []providerRecordResponse `json:"providers"`
}

// providersByCID handles GET /v1/providers/{cid}.
func (d *providerDirectory) providersByCID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	entries := d.list(cid, time.Now())
	providers := make([]providerRecordResponse, 0, len(entries))
	for _, e := range entries {
		providers = append(providers, providerRecordResponse{
			ProviderID: e.providerID,
			Addresses:  e.addresses,
			ExpiresAt:  e.expiresAt,
		})
	}
	writeJSON(w, http.StatusOK, providerListResponse{CID: cid, Providers: providers})
}

// providerByID handles PUT and DELETE on /v1/providers/{cid}/{providerId}.
func (d *providerDirectory) providerByID(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		d.putProvider(w, r)
	case http.MethodDelete:
		d.deleteProvider(w, r)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

// putProvider handles PUT /v1/providers/{cid}/{providerId}.
func (d *providerDirectory) putProvider(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	providerID := r.PathValue("providerId")
	if !validProviderID(providerID) {
		writeError(w, http.StatusBadRequest, "invalid_provider")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderBodySize+1))
	if err != nil || len(body) > maxProviderBodySize {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !wellFormedJSON(body) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var req providerPutRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.Addresses == nil || req.ExpiresAt == nil ||
		bytes.Equal(bytes.TrimSpace(*req.Addresses), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(*req.ExpiresAt), []byte("null")) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var rawAddresses []json.RawMessage
	var expiresAtText string
	if err := json.Unmarshal(*req.Addresses, &rawAddresses); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	addresses := make([]string, len(rawAddresses))
	for i, raw := range rawAddresses {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '"' {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := json.Unmarshal(trimmed, &addresses[i]); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	if err := json.Unmarshal(*req.ExpiresAt, &expiresAtText); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !validAddresses(addresses) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_address")
		return
	}
	now := time.Now()
	expiresAt, err := time.Parse(time.RFC3339, expiresAtText)
	if err != nil || !expiresAt.After(now) || expiresAt.After(now.Add(maxProviderTTL)) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_expiration")
		return
	}
	created := d.put(cid, providerID, addresses, expiresAt, now)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, providerRecordResponse{
		ProviderID: providerID,
		Addresses:  addresses,
		ExpiresAt:  expiresAt,
	})
}

// deleteProvider handles DELETE /v1/providers/{cid}/{providerId}.
func (d *providerDirectory) deleteProvider(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "invalid_cid")
		return
	}
	providerID := r.PathValue("providerId")
	if !validProviderID(providerID) {
		writeError(w, http.StatusBadRequest, "invalid_provider")
		return
	}
	if !d.remove(cid, providerID, time.Now()) {
		writeError(w, http.StatusNotFound, "provider_not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validProviderID reports whether s is 1 to 64 lowercase letters, digits or
// hyphens, beginning and ending with a letter or digit.
func validProviderID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// validAddresses reports whether addrs is a non-empty list of at most
// maxProviderAddresses distinct absolute http(s) URLs without userinfo,
// fragment or empty host, strictly sorted as strings.
func validAddresses(addrs []string) bool {
	if len(addrs) < 1 || len(addrs) > maxProviderAddresses {
		return false
	}
	prev := ""
	for i, a := range addrs {
		if i > 0 && a <= prev {
			return false
		}
		prev = a
		u, err := url.Parse(a)
		if err != nil {
			return false
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return false
		}
		if u.Hostname() == "" || u.User != nil {
			return false
		}
		// A literal '#' starts a fragment; reject even an empty one.
		if strings.Contains(a, "#") {
			return false
		}
	}
	return true
}
