// Package server exposes the frozen public surface of CidVault.
//
// The baseline only reports process health. Later work adds the capabilities
// described in README.md; keep the exported surface here backward compatible.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Version is the baseline release identifier.
const Version = "0.1.0"

type health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// Handler returns the HTTP surface served by the baseline.
func Handler() http.Handler {
	audit := newAuditLog()
	objects := newStore()
	objects.audit = audit
	providers := newProviderDirectory()
	providers.audit = audit
	retriever := newRetriever(objects, providers)
	sealed := newSealedStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"code":"method_not_allowed"}}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(health{Status: "ok", Service: "cidvault", Version: Version})
	})
	mux.HandleFunc("/v1/objects", objects.postObject)
	mux.HandleFunc("/v1/objects/{cid}", objects.getObject)
	mux.HandleFunc("/v1/objects/{cid}/manifest", objects.getManifest)
	mux.HandleFunc("/v1/objects/{cid}/metadata", objects.metadataByID)
	mux.HandleFunc("/v1/blocks/{cid}", objects.getBlockByID)
	mux.HandleFunc("/v1/bundles", objects.postBundle)
	mux.HandleFunc("/v1/bundles/{cid}", objects.getBundle)
	mux.HandleFunc("/v1/delta-bundles", objects.postDeltaBundle)
	mux.HandleFunc("/v1/delta-bundles/{cid}", objects.postDeltaBundleByID)
	mux.HandleFunc("/v1/storage/stats", objects.getStorageStats)
	mux.HandleFunc("/v1/pins", objects.getPins)
	mux.HandleFunc("/v1/pins/{cid}", objects.pinByID)
	mux.HandleFunc("/v1/gc", objects.gc)
	mux.HandleFunc("/v1/providers/{cid}", providers.providersByCID)
	mux.HandleFunc("/v1/providers/{cid}/{providerId}", providers.providerByID)
	mux.HandleFunc("/v1/retrievals/{cid}", retriever.postRetrieval)
	mux.HandleFunc("/v1/retrievability-proofs/{cid}", objects.postRetrievabilityProof)
	mux.HandleFunc("/v1/sealed-objects", sealed.postSealedObject)
	mux.HandleFunc("/v1/sealed-objects/{cid}", sealed.sealedObjectByID)
	mux.HandleFunc("/v1/sealed-objects/{cid}/envelope", sealed.getSealedEnvelope)
	mux.HandleFunc("/v1/audit/events", audit.getEvents)

	// The gateway is dispatched before ServeMux on the raw request target:
	// ServeMux cleans "."/".." and empty segments with redirects and hands
	// handlers already-percent-decoded path values, both of which would
	// violate the gateway's exact single-decoding path contract. The named
	// reference routes use the same raw dispatch so each name is decoded
	// exactly once and an encoded slash cannot be rewritten into another
	// route.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case hasGatewayPrefix(r):
			objects.gateway(w, r)
		case hasRefsPrefix(r):
			objects.refsDispatch(w, r)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

// hasGatewayPrefix reports whether the request target names a gateway route.
// The raw target is inspected (query stripped) so percent-encoded slashes and
// dots survive untouched.
func hasGatewayPrefix(r *http.Request) bool {
	target := r.RequestURI
	if target == "" {
		target = r.URL.EscapedPath()
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	return strings.HasPrefix(target, gatewayPrefix)
}

// hasRefsPrefix reports whether the request target names a named-reference
// route. As with the gateway, the raw target is inspected so the name is
// percent-decoded exactly once by the dispatcher.
func hasRefsPrefix(r *http.Request) bool {
	target := r.RequestURI
	if target == "" {
		target = r.URL.EscapedPath()
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	return strings.HasPrefix(target, refsPrefix)
}
