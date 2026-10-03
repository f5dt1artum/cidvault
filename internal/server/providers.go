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

// maxProviderBody bounds the wire size of a provider publication body. The
// body holds at most 16 URLs, so a megabyte is ample slack.
const maxProviderBody = 1 << 20

// maxProviderAddresses is the largest number of distinct addresses accepted
// in one publication.
const maxProviderAddresses = 16

// maxProviderTTL bounds how far in the future a publication may expire.
const maxProviderTTL = 24 * time.Hour

// providerRecord is one node's published access locations for a content
// identifier. Records live in process memory only: they take no part in
// pinning, garbage collection or storage statistics and are not preserved
// across restarts.
type providerRecord struct {
	addresses []string
	expiresAt time.Time
}

// validProvider reports whether the record is in force at now.
func (p providerRecord) validProvider(now time.Time) bool {
	return p.expiresAt.After(now)
}

// providerDirectory maps content identifiers to per-node publication
// records. The directory never requires the object to be stored locally.
// All checks and mutations happen under one lock, so publication, lookup
// and deletion observe one consistent instant and concurrent first
// publications for one key are serialized.
type providerDirectory struct {
	mu      sync.RWMutex
	records map[string]map[string]providerRecord
}

func newProviderDirectory() *providerDirectory {
	return &providerDirectory{records: make(map[string]map[string]providerRecord)}
}

// put publishes or replaces the record for cid/providerID. It reports
// created == true when no valid record existed at now (absent or already
// expired). The check and the write are one atomic operation.
func (d *providerDirectory) put(cid, providerID string, addresses []string, expiresAt, now time.Time) (providerRecord, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	byCID := d.records[cid]
	if byCID == nil {
		byCID = make(map[string]providerRecord)
		d.records[cid] = byCID
	}
	rec := providerRecord{
		addresses: append([]string{}, addresses...),
		expiresAt: expiresAt,
	}
	existing, found := byCID[providerID]
	created := !found || !existing.validProvider(now)
	byCID[providerID] = rec
	return rec, created
}

// remove deletes the record for cid/providerID, reporting whether a valid
// record existed at now. Expired records are removed lazily and reported
// as absent.
func (d *providerDirectory) remove(cid, providerID string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	byCID := d.records[cid]
	rec, found := byCID[providerID]
	if !found || !rec.validProvider(now) {
		delete(byCID, providerID)
		return false
	}
	delete(byCID, providerID)
	if len(byCID) == 0 {
		delete(d.records, cid)
	}
	return true
}

// providerEntry is one valid record as seen by a snapshot listing.
type providerEntry struct {
	providerID string
	addresses  []string
	expiresAt  time.Time
}

// list returns the records in force for cid at now, sorted by providerID.
// The slice is taken under one read-lock hold and every address slice is
// copied, so the result is a point-in-time snapshot the caller owns.
func (d *providerDirectory) list(cid string, now time.Time) []providerEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	entries := []providerEntry{}
	for providerID, rec := range d.records[cid] {
		if !rec.validProvider(now) {
			continue
		}
		entries = append(entries, providerEntry{
			providerID: providerID,
			addresses:  append([]string{}, rec.addresses...),
			expiresAt:  rec.expiresAt,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].providerID < entries[j].providerID })
	return entries
}

// providerResponse is the JSON encoding of one published record.
type providerResponse struct {
	ProviderID string    `json:"providerId"`
	Addresses  []string  `json:"addresses"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// providerListResponse is the JSON body of GET /v1/providers/{cid}.
type providerListResponse struct {
	CID       string             `json:"cid"`
	Providers []providerResponse `json:"providers"`
}

// providerPutRequest is the strictly decoded body of PUT
// /v1/providers/{cid}/{providerId}. Pointer fields distinguish a missing
// or null key from a value; address elements are pointers so null entries
// count as type errors.
type providerPutRequest struct {
	Addresses *[]*string `json:"addresses"`
	ExpiresAt *string    `json:"expiresAt"`
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
	providers := make([]providerResponse, 0, len(entries))
	for _, e := range entries {
		providers = append(providers, providerResponse{
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
	now := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderBody+1))
	if err != nil || len(body) > maxProviderBody {
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
	// A missing key or a null value leaves the pointer nil.
	if req.Addresses == nil || req.ExpiresAt == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	rawAddresses := *req.Addresses
	if rawAddresses == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	if len(rawAddresses) < 1 || len(rawAddresses) > maxProviderAddresses {
		writeError(w, http.StatusUnprocessableEntity, "invalid_address")
		return
	}
	seen := make(map[string]bool, len(rawAddresses))
	addresses := make([]string, 0, len(rawAddresses))
	for _, a := range rawAddresses {
		// A null element is a type error; a bad or repeated string is a
		// non-compliant address.
		if a == nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if seen[*a] || !validProviderAddress(*a) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_address")
			return
		}
		seen[*a] = true
		addresses = append(addresses, *a)
	}
	sort.Strings(addresses)

	expiresAt, err := time.Parse(time.RFC3339, *req.ExpiresAt)
	if err != nil || !expiresAt.After(now) || expiresAt.After(now.Add(maxProviderTTL)) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_expiration")
		return
	}

	rec, created := d.put(cid, providerID, addresses, expiresAt, now)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, providerResponse{
		ProviderID: providerID,
		Addresses:  rec.addresses,
		ExpiresAt:  rec.expiresAt,
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

// validProviderID reports whether id is 1 to 64 lowercase letters, digits
// or hyphens, beginning and ending with a letter or digit.
func validProviderID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return isProviderIDAlnum(id[0]) && isProviderIDAlnum(id[len(id)-1])
}

func isProviderIDAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// validProviderAddress reports whether raw is an absolute http or https URL
// without user information, a fragment or an empty host.
func validProviderAddress(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	if u.User != nil || u.Host == "" || u.Hostname() == "" {
		return false
	}
	// A literal "#" always introduces the fragment; an encoded "%23" is
	// ordinary path text and does not match.
	if strings.Contains(raw, "#") {
		return false
	}
	return true
}
