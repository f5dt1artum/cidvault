package server

import (
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// auditCapacity bounds the retained event history. Once the log is full the
// oldest events are evicted while sequence numbers keep increasing and are
// never reused.
const auditCapacity = 10000

// Audit actions, one per recorded entry point.
const (
	auditObjectUpload      = "object.upload"
	auditBundleImport      = "bundle.import"
	auditDeltaBundleImport = "delta_bundle.import"
	auditPinPut            = "pin.put"
	auditPinDelete         = "pin.delete"
	auditGCPreview         = "gc.preview"
	auditGCCollect         = "gc.collect"
	auditProviderPut       = "provider.put"
	auditProviderDelete    = "provider.delete"
	auditRetrievalLocal    = "retrieval.local"
	auditRetrievalFetch    = "retrieval.fetch"
)

// auditEvent is one recorded outcome of a successful request. Fields that
// do not apply to an action are nil and serialize as null.
type auditEvent struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	Action     string    `json:"action"`
	CID        *string   `json:"cid"`
	Result     string    `json:"result"`
	Objects    *int      `json:"objects"`
	Bytes      *int      `json:"bytes"`
	ProviderID *string   `json:"providerId"`
}

// auditRecord carries the commit-time facts of one event; the log assigns
// the sequence number and timestamp when the event is appended.
type auditRecord struct {
	action     string
	result     string
	cid        *string
	objects    *int
	bytes      *int
	providerID *string
}

// objectAuditRecord builds the record for an action that stores an object:
// bytes is the object body length and the result names whether this request
// created the object. Remote fetches use the retrieved_* result names.
func objectAuditRecord(action string, obj *object, created bool, providerID *string) auditRecord {
	result := "existing"
	if created {
		result = "created"
	}
	if action == auditRetrievalFetch {
		result = "retrieved_existing"
		if created {
			result = "retrieved_created"
		}
	}
	return auditRecord{
		action:     action,
		result:     result,
		cid:        strPtr(obj.cid),
		bytes:      intPtr(obj.size),
		providerID: providerID,
	}
}

func strPtr(s string) *string { return &s }
func intPtr(v int) *int       { return &v }

// auditLog is a process-local ring of recent audit events. Appends happen
// under the caller's business lock so the event order matches the commit
// order of the recorded operations. The log takes no part in pinning,
// garbage collection, storage statistics or content addressing, and is not
// retained across restarts.
type auditLog struct {
	mu     sync.Mutex
	ring   []auditEvent
	start  int // ring index of the oldest retained event
	length int
	next   int64 // next sequence number to assign
}

func newAuditLog() *auditLog {
	return &auditLog{ring: make([]auditEvent, auditCapacity), next: 1}
}

// append records one event with the next sequence number, evicting the
// oldest event once the log is full. A nil log discards the record, so
// business code needs no nil checks of its own.
func (a *auditLog) append(rec auditRecord) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ev := auditEvent{
		Seq:        a.next,
		At:         time.Now().UTC(),
		Action:     rec.action,
		CID:        rec.cid,
		Result:     rec.result,
		Objects:    rec.objects,
		Bytes:      rec.bytes,
		ProviderID: rec.providerID,
	}
	a.next++
	if a.length < auditCapacity {
		a.ring[(a.start+a.length)%auditCapacity] = ev
		a.length++
		return
	}
	a.ring[a.start] = ev
	a.start = (a.start + 1) % auditCapacity
}

// snapshot returns up to limit retained events with seq > after in
// ascending seq order. expired reports a non-zero after that points before
// the predecessor of the oldest retained event. hasMore reports whether the
// retained history holds events beyond the returned page.
func (a *auditLog) snapshot(after int64, limit int) (events []auditEvent, hasMore, expired bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	events = []auditEvent{}
	if a.length == 0 {
		return events, false, false
	}
	if after != 0 && after < a.ring[a.start].Seq-1 {
		return nil, false, true
	}
	for i := 0; i < a.length; i++ {
		ev := a.ring[(a.start+i)%auditCapacity]
		if ev.Seq <= after {
			continue
		}
		if len(events) == limit {
			hasMore = true
			break
		}
		events = append(events, ev)
	}
	return events, hasMore, false
}

// auditResponse is the JSON body of GET /v1/audit/events.
type auditResponse struct {
	Events    []auditEvent `json:"events"`
	NextAfter int64        `json:"nextAfter"`
	HasMore   bool         `json:"hasMore"`
}

// getEvents handles GET /v1/audit/events. Reads are not themselves audited.
func (a *auditLog) getEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	after, limit := int64(0), 100
	for key, values := range query {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		switch key {
		case "after":
			v, ok := parseAuditInt(values[0])
			if !ok {
				writeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			after = v
		case "limit":
			v, ok := parseAuditInt(values[0])
			if !ok || v < 1 || v > 1000 {
				writeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			limit = int(v)
		default:
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	events, hasMore, expired := a.snapshot(after, limit)
	if expired {
		writeError(w, http.StatusGone, "audit_cursor_expired")
		return
	}
	nextAfter := after
	if len(events) > 0 {
		nextAfter = events[len(events)-1].Seq
	}
	writeJSON(w, http.StatusOK, auditResponse{
		Events:    events,
		NextAfter: nextAfter,
		HasMore:   hasMore,
	})
}

// parseAuditInt parses a non-negative decimal integer with no sign or other
// decoration; anything else, including values beyond int64 range, is
// rejected.
func parseAuditInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
