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

// replicationTimeout bounds a single target delivery, including the
// response body read.
const replicationTimeout = 5 * time.Second

// maxReplicationTargets bounds the targets one replication request may carry.
const maxReplicationTargets = 16

// maxReplicationBodySize bounds the wire size of a replication request body.
// The protocol itself only bounds the target count; the cap just keeps an
// unbounded request from exhausting memory.
const maxReplicationBodySize = 1 << 20

// maxReplicationResponseSize bounds the wire size of one target's import
// response. A conforming response is a small JSON document; the cap keeps a
// misbehaving target from exhausting memory.
const maxReplicationResponseSize = 1 << 20

// replicator pushes a locally stored object to remote import endpoints. It
// is deliberately one-directional: delivery changes no local business state
// and appends no local audit events; each remote applies its own import,
// deduplication and audit semantics.
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

// replicationRequest is the strictly decoded body of
// POST /v1/replications/{cid}. Pointer fields distinguish a missing key
// (and an explicit null) from a zero value; both stay raw so non-string
// elements, quoted numbers and fractional values are rejected as type
// errors rather than value errors.
type replicationRequest struct {
	Targets  *json.RawMessage `json:"targets"`
	Required *json.RawMessage `json:"required"`
}

// replicationResult is one target's outcome, reported in target order.
type replicationResult struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Result string `json:"result"`
}

// replicationResponse is the JSON body of POST /v1/replications/{cid}.
type replicationResponse struct {
	CID            string              `json:"cid"`
	Required       int                 `json:"required"`
	Succeeded      int                 `json:"succeeded"`
	Failed         int                 `json:"failed"`
	MetRequirement bool                `json:"metRequirement"`
	Results        []replicationResult `json:"results"`
}

// postReplication handles POST /v1/replications/{cid}: it exports the
// acceptance-time local snapshot of the object as one complete bundle and
// delivers the same bytes to every target in parallel.
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
	obj := rp.objects.get(cid)
	if obj == nil {
		writeError(w, http.StatusNotFound, "object_not_found")
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
	// The object is immutable once stored, so the bundle built here is the
	// acceptance-time snapshot; every target receives the same bytes.
	bundle := bundleBytes(obj)
	results := make([]replicationResult, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A failed or slow target neither cancels nor blocks the
			// others beyond its own delivery.
			results[i] = rp.deliver(target, cid, bundle)
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

// parseReplicationRequest strictly validates the request body. On failure it
// returns the error code and HTTP status to report; no target is contacted
// on the failure path.
func parseReplicationRequest(body []byte) (targets []string, required int, code string, status int) {
	invalid := func() ([]string, int, string, int) {
		return nil, 0, "invalid_request", http.StatusBadRequest
	}
	if !wellFormedJSON(body) {
		return invalid()
	}
	var req replicationRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Targets == nil || req.Required == nil {
		return invalid()
	}
	if bytes.Equal(bytes.TrimSpace(*req.Targets), []byte("null")) {
		return invalid()
	}
	var rawTargets []json.RawMessage
	if err := json.Unmarshal(*req.Targets, &rawTargets); err != nil {
		return invalid()
	}
	targets = make([]string, len(rawTargets))
	for i, raw := range rawTargets {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return invalid()
		}
		if err := json.Unmarshal(trimmed, &targets[i]); err != nil {
			return invalid()
		}
	}

	// Required must be a JSON integer; an integer outside int64 range is a
	// value problem, not a type problem: it cannot name a supported count.
	n, err := parseInt(*req.Required)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
		}
		return invalid()
	}
	if !validReplicationTargets(targets) || n < 1 || n > len(targets) {
		return nil, 0, "invalid_replication_request", http.StatusUnprocessableEntity
	}
	return targets, n, "", 0
}

// validReplicationTargets reports whether targets is a non-empty list of at
// most maxReplicationTargets distinct absolute http(s) URLs, strictly sorted
// as strings, each with the path exactly /v1/bundles and no userinfo, query
// or fragment.
func validReplicationTargets(targets []string) bool {
	if len(targets) < 1 || len(targets) > maxReplicationTargets {
		return false
	}
	prev := ""
	for i, t := range targets {
		if i > 0 && t <= prev {
			return false
		}
		prev = t
		u, err := url.Parse(t)
		if err != nil {
			return false
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return false
		}
		if u.Hostname() == "" || u.User != nil {
			return false
		}
		// The escaped path must be exactly /v1/bundles: percent-encoded
		// spellings of the same decoded path do not qualify.
		if u.EscapedPath() != "/v1/bundles" {
			return false
		}
		if u.ForceQuery || u.RawQuery != "" {
			return false
		}
		// A literal '#' starts a fragment; reject even an empty one.
		if strings.Contains(t, "#") {
			return false
		}
	}
	return true
}

// deliver POSTs the bundle to one target and classifies the outcome. The
// request carries no caller credentials and follows no redirects.
func (rp *replicator) deliver(target, cid string, bundle []byte) replicationResult {
	result := replicationResult{Target: target}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(bundle))
	if err != nil {
		result.Result = "unreachable"
		return result
	}
	req.Header.Set("Content-Type", bundleMediaType)
	resp, err := rp.client.Do(req)
	if err != nil {
		// Connection failures and the per-target timeout both land here.
		result.Result = "unreachable"
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		result.Result = "rejected"
		return result
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReplicationResponseSize+1))
	if err != nil || len(body) > maxReplicationResponseSize {
		result.Result = "rejected"
		return result
	}
	created, ok := parseBundleImportResult(body, cid)
	if !ok {
		result.Result = "rejected"
		return result
	}
	result.OK = true
	if created {
		result.Result = "created"
	} else {
		result.Result = "existing"
	}
	return result
}

// bundleImportResult is the contract-checked subset of a target's import
// response.
type bundleImportResult struct {
	CID     *string `json:"cid"`
	Created *bool   `json:"created"`
}

// parseBundleImportResult reports whether body is a conforming import
// response for the source cid: a single well-formed JSON document whose cid
// equals the source CID and whose created is a boolean.
func parseBundleImportResult(body []byte, cid string) (created, ok bool) {
	if !wellFormedJSON(body) {
		return false, false
	}
	var res bundleImportResult
	if err := json.Unmarshal(body, &res); err != nil {
		return false, false
	}
	if res.CID == nil || res.Created == nil || *res.CID != cid {
		return false, false
	}
	return *res.Created, true
}
