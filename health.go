package main

import (
	"net/http"
	"sync/atomic"
)

// Readiness reports local ability to accept work, not remote SMB availability.
// Startup validates configuration before binding. Drain flips readiness before
// the listener shuts down; liveness never depends on shared storage health.
type gatewayHealth struct{ draining atomic.Bool }

func (h *gatewayHealth) drain() { h.draining.Store(true) }
func (h *gatewayHealth) handler(ready bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		status, body := http.StatusOK, "ok\n"
		if ready && h.draining.Load() {
			status, body = http.StatusServiceUnavailable, "draining\n"
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	})
}
func (c Config) probeHealth() *gatewayHealth {
	if c.health != nil {
		return c.health
	}
	return &gatewayHealth{}
}
