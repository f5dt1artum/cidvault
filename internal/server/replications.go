package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bundleImportPath is the only path a replication target may carry: it is
// the collection endpoint that imports full offline bundles.
const bundleImportPath = "/v1/bundles"

// maxReplicationTargets bounds the targets of one replication request.
const maxReplicationTargets = 16

// maxReplicationBodySize bounds the wire size of a replication request. The
// protocol itself only bounds the target count; the cap just keeps an
// unbounded request from exhausting memory.
const maxReplicationBodySize = 1 << 20

// maxReplicationResponseSize bounds the JSON import response accepted from a
// target. A conformant response only carries identifiers and a boolean, so a
// larger body cannot be a valid answer.
const maxReplicationResponseSize = 1 << 20

// replicationTimeout bounds a single target delivery, including the body
// read.
const replicationTimeout = 5 * time.Second

// replicationRequest is the strictly decoded body of
// POST /v1/replications/{cid}. The fields stay raw so quoted targets, a
// fractional required value and similar type errors are rejected as
// malformed requests rather than value violations.
type replicationRequest struct {
	Targets  *json.RawMessage `json:"targets"`
	Required *json.RawMessage `json:"required"`
}

// replicationResultEntry is one target outcome, reported in the same order
// the targets were given.
type replicationResultEntry struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Result string `json:"result"`
}

// replicationResponse is the JSON body of POST /v1/replications/{cid}.
type replicationResponse struct {
	CID            string                   `json:"cid"`
	Required       int                      `json:"required"`
	Succeeded      int                      `json:"succeeded"`
	Failed         int                      `json:"failed"`
	MetRequirement bool                     `json:"metRequirement"`
	Results        []replicationResultEntry `json:"results"`
}

// replicator delivers one local object's full offline bundle to several
// bundle endpoints in parallel. It never mutates local state and appends no
// audit events; the targets apply their ordinary import, deduplication and
// audit semantics.
type replicator struct {
	objects *store
	client  *http.Client
}

func newReplicator(objects *store) *replicator {
	return &replicator{
		objects: objects,
		client: &http.Client{
			Timeout: replicationTimeout,
			// Redirects are not followed: a 3xx response rejects the target.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// postReplication handles POST /v1/replications/{cid}.
func (rp *replicator) postReplication(w http.ResponseWriter, r *http.Request) {
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
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReplicationBodySize+1))
	if err != nil || len(body) > maxReplicationBodySize {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	targets, required, code, status := parseReplicationRequest(body)
	if code != "" {
		writeError(w, status, code)
		return
	}
	// Every structural and value failure above is settled before the object
	// is resolved and before any target is contacted.
	obj := rp.objects.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
		return
	}

	// Build the offline package once from the acceptance-time snapshot and
	// send that identical package to every target.
	payload, err := json.Marshal(exportBundle(obj))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	results := make([]replicationResultEntry, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, result := rp.deliver(target, payload, cid)
			results[i] = replicationResultEntry{Target: target, OK: ok, Result: result}
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, res := range results {
		if res.OK {
			succeeded++
		}
	}
	writeJSON(w, http.StatusOK, replicationResponse{
		CID:            cid,
		Required:       required,
		Succeeded:      succeeded,
		Failed:         len(targets) - succeeded,
		MetRequirement: succeeded >= required,
		Results:        results,
	})
}

// parseReplicationRequest strictly validates the request body. Structural
// problems (non-JSON, missing, unknown or duplicate fields, wrong types)
// return invalid_request; target and required value violations return
// invalid_replication_request. Nothing past this point touches the network.
func parseReplicationRequest(body []byte) (targets []string, required int, code string, status int) {
	if !wellFormedJSON(body) {
		return nil, 0, "invalid_request", http.StatusBadRequest
	}
	var req replicationRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Targets == nil || req.Required == nil ||
		bytes.Equal(bytes.TrimSpace(*req.Targets), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(*req.Required), []byte("null")) {
		return nil, 0, "invalid_request", http.StatusBadRequest
	}

	var rawTargets []json.RawMessage
	if err := json.Unmarshal(*req.Targets, &rawTargets); err != nil {
		return nil, 0, "invalid_request", http.StatusBadRequest
	}
	targets = make([]string, len(rawTargets))
	for i, raw := range rawTargets {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || trimmed[0] != '"' {
			return nil, 0, "invalid_request", http.StatusBadRequest
		}
		if err := json.Unmarshal(trimmed, &targets[i]); err != nil {
			return nil, 0, "invalid_request", http.StatusBadRequest
		}
	}

	n, err := parseInt(*req.Required)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
		}
		return nil, 0, "invalid_request", http.StatusBadRequest
	}
	if len(targets) < 1 || len(targets) > maxReplicationTargets || n < 1 || n > len(targets) {
		return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
	}
	prev := ""
	for i, t := range targets {
		// Equal neighbours are duplicates; a smaller neighbour breaks the
		// strict ascending order. Both are value violations.
		if i > 0 && t <= prev {
			return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
		}
		prev = t
		if !validReplicationTarget(t) {
			return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
		}
	}
	return targets, n, "", 0
}

// validReplicationTarget reports whether raw is an absolute http(s) URL
// whose path is exactly the bundle import endpoint, carrying no userinfo,
// query or fragment.
func validReplicationTarget(raw string) bool {
	// An unencoded '?' or '#' can only introduce a query or fragment; reject
	// them literally, including an otherwise-empty trailing separator.
	if strings.ContainsAny(raw, "?#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return false
	}
	// RawPath is empty when the raw path needs no percent-decoding at all; a
	// set RawPath means an encoded variant of "/v1/bundles", which is not the
	// exact required path.
	if u.RawPath != "" || u.Path != bundleImportPath {
		return false
	}
	return true
}

// bundleImportReply is the subset of the POST /v1/bundles response that
// decides success: the returned cid must name the source object and created
// must be a JSON boolean.
type bundleImportReply struct {
	CID     *string `json:"cid"`
	Created *bool   `json:"created"`
}

// deliver POSTs one identical bundle package to target without caller
// credentials and without following redirects. It reports ok with "created"
// or "existing" on a conformant 200/201, "unreachable" on a transport error
// or timeout, and "rejected" for redirects, any other status or a response
// that does not satisfy the import contract.
func (rp *replicator) deliver(target string, payload []byte, sourceCID string) (ok bool, result string) {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return false, "unreachable"
	}
	// Only the bundle media type is sent; no inbound Authorization or other
	// caller header is forwarded.
	req.Header.Set("Content-Type", bundleMediaType)
	resp, err := rp.client.Do(req)
	if err != nil {
		return false, "unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return false, "rejected"
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplicationResponseSize+1))
	if err != nil || len(raw) > maxReplicationResponseSize {
		return false, "rejected"
	}
	var reply bundleImportReply
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&reply); err != nil {
		return false, "rejected"
	}
	// Exactly one JSON value is accepted; trailing content breaks the
	// contract.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return false, "rejected"
	}
	if reply.CID == nil || reply.Created == nil || *reply.CID != sourceCID {
		return false, "rejected"
	}
	if *reply.Created {
		return true, "created"
	}
	return true, "existing"
}
