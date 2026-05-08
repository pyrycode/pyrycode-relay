package relay

import (
	"encoding/json"
	"net/http"
	"time"
)

// healthzResponse is the JSON shape returned by the /healthz handler. Field
// order is significant: encoding/json marshals in declaration order and the
// on-the-wire key order is part of the public contract.
type healthzResponse struct {
	Status            string `json:"status"`
	Version           string `json:"version"`
	ConnectedBinaries int    `json:"connected_binaries"`
	ConnectedPhones   int    `json:"connected_phones"`
	UptimeSeconds     int64  `json:"uptime_seconds"`
}

// NewHealthzHandler returns an http.Handler that responds to every request
// with a small JSON body containing the relay's status, version, current
// connection counts, and uptime. The handler is intended to be served
// unauthenticated on /healthz: aggregate counts are exposed to anonymous
// callers as an explicit operational tradeoff (see ticket #10).
//
// reg is the live connection registry; the handler reads it via Counts() on
// every request. version is the build-time version string (the same value
// --version prints). startedAt is the moment the relay began serving
// requests; uptime is reported as time.Since(startedAt) rounded to whole
// seconds, floored at zero.
//
// Safe for concurrent use: holds no per-request state, spawns no goroutines,
// performs one read-locked Counts() call per request and releases the lock
// before any response I/O.
func NewHealthzHandler(reg *Registry, version string, startedAt time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		binaries, phones := reg.Counts()
		uptime := int64(time.Since(startedAt).Seconds())
		if uptime < 0 {
			uptime = 0
		}

		// json.Marshal of a fixed-shape struct of primitives cannot fail.
		body, _ := json.Marshal(healthzResponse{
			Status:            "ok",
			Version:           version,
			ConnectedBinaries: binaries,
			ConnectedPhones:   phones,
			UptimeSeconds:     uptime,
		})

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
