// Package server exposes the frozen public surface of CidVault.
//
// The baseline only reports process health. Later work adds the capabilities
// described in README.md; keep the exported surface here backward compatible.
package server

import (
	"encoding/json"
	"net/http"
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
	objects := newStore()
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
	mux.HandleFunc("/v1/pins", objects.getPins)
	mux.HandleFunc("/v1/pins/{cid}", objects.pinByID)
	mux.HandleFunc("/v1/gc", objects.gc)
	return mux
}
