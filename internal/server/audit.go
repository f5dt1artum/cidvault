package server

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// auditCapacity bounds the retained event history. Once the log grows past
// the capacity the oldest events are evicted; sequence numbers are never
// reused and keep increasing across evictions.
const auditCapacity = 10000

// The audit actions recorded by the mutation and retrieval entry points.
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

// auditEvent is one committed mutation or successful retrieval. Fields that
// do not apply to an action are null.
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

// objectAuditEvent builds the event for an action that stores or serves one
// object body: objects is 1 and bytes is the body length.
func objectAuditEvent(action, result, cid string, size int, providerID *string) auditEvent {
	objects := 1
	bytes := size
	return auditEvent{
		Action:     action,
		CID:        &cid,
		Result:     result,
		Objects:    &objects,
		Bytes:      &bytes,
		ProviderID: providerID,
	}
}

// pinAuditEvent builds the event for a pin mutation on one object.
func pinAuditEvent(action, result, cid string) auditEvent {
	objects := 1
	return auditEvent{Action: action, CID: &cid, Result: result, Objects: &objects}
}

// gcAuditEvent builds the event for a garbage-collection run: no single cid,
// objects and bytes mirror the response totals.
func gcAuditEvent(action, result string, objects, bytes int) auditEvent {
	return auditEvent{Action: action, Result: result, Objects: &objects, Bytes: &bytes}
}

// providerAuditEvent builds the event for a provider directory mutation.
func providerAuditEvent(action, result, cid, providerID string) auditEvent {
	return auditEvent{Action: action, CID: &cid, Result: result, ProviderID: &providerID}
}

// objectResult maps an object commit to its audit result token.
func objectResult(action string, created bool) string {
	if action == auditRetrievalFetch {
		if created {
			return "retrieved_created"
		}
		return "retrieved_existing"
	}
	if created {
		return "created"
	}
	return "existing"
}

// auditLog is the process-local audit event stream. Producers append events
// after the business state commits, while still holding the lock that
// committed the change, so the event order matches the commit order under
// concurrency. The log lives in process memory only and is not retained
// across restarts.
type auditLog struct {
	mu      sync.Mutex
	events  []auditEvent // ascending seq, oldest first
	nextSeq int64
}

func newAuditLog() *auditLog {
	return &auditLog{nextSeq: 1}
}

// append stamps one event with its sequence number and UTC commit time and
// records it, evicting the oldest events beyond the retention capacity.
func (l *auditLog) append(e auditEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = l.nextSeq
	l.nextSeq++
	e.At = time.Now().UTC()
	l.events = append(l.events, e)
	if len(l.events) > auditCapacity {
		excess := len(l.events) - auditCapacity
		l.events = append(l.events[:0], l.events[excess:]...)
	}
}

// page snapshots the retained events with seq greater than after, returning
// at most limit of them in ascending seq order. expired reports that a
// non-zero cursor predates the seq before the oldest retained event.
// nextAfter is the seq of the last event in the page, or after when the page
// is empty; hasMore reports whether the snapshot holds events beyond the
// page.
func (l *auditLog) page(after int64, limit int) (events []auditEvent, nextAfter int64, hasMore, expired bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	events = []auditEvent{}
	nextAfter = after
	if len(l.events) == 0 {
		return events, nextAfter, false, false
	}
	if after != 0 && after < l.events[0].Seq-1 {
		return nil, 0, false, true
	}
	start := sort.Search(len(l.events), func(i int) bool { return l.events[i].Seq > after })
	rest := l.events[start:]
	n := limit
	if n > len(rest) {
		n = len(rest)
	}
	events = append(events, rest[:n]...)
	if n > 0 {
		nextAfter = events[n-1].Seq
	}
	return events, nextAfter, len(rest) > n, false
}

// auditEventsResponse is the JSON body of GET /v1/audit/events.
type auditEventsResponse struct {
	Events    []auditEvent `json:"events"`
	NextAfter int64        `json:"nextAfter"`
	HasMore   bool         `json:"hasMore"`
}

// getEvents handles GET /v1/audit/events.
func (l *auditLog) getEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	query := r.URL.Query()
	for key := range query {
		if key != "after" && key != "limit" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	after := int64(0)
	if values, present := query["after"]; present {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		v, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		after = v
	}
	limit := 100
	if values, present := query["limit"]; present {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		v, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || v < 1 || v > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		limit = int(v)
	}
	events, nextAfter, hasMore, expired := l.page(after, limit)
	if expired {
		writeError(w, http.StatusGone, "audit_cursor_expired")
		return
	}
	writeJSON(w, http.StatusOK, auditEventsResponse{
		Events:    events,
		NextAfter: nextAfter,
		HasMore:   hasMore,
	})
}
