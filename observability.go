package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jasonjiang8866/smbfileserver/client"
)

type requestIDKey struct{}

// Always generate our own ID. Incoming headers are untrusted, may contain
// secrets, and must not become arbitrary log values or metric labels.
func newRequestID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("request ID entropy unavailable")
	}
	return hex.EncodeToString(id[:])
}

type requestKey struct{ route, method, status, outcome string }
type routeTotals struct {
	count, bytes uint64
	seconds      float64
	buckets      [8]uint64
}

var durationBounds = [8]float64{.005, .025, .1, .5, 1, 5, 15, 60}

type observability struct {
	logger   *slog.Logger
	stats    func() client.Stats
	active   atomic.Int64
	mu       sync.Mutex
	requests map[requestKey]uint64
	routes   map[string]*routeTotals
}

func newObservability(logger *slog.Logger, stats func() client.Stats) *observability {
	return &observability{logger: logger, stats: stats, requests: make(map[requestKey]uint64), routes: make(map[string]*routeTotals)}
}
func (c Config) observer() *observability {
	if c.observation != nil {
		return c.observation
	}
	return newObservability(slog.Default(), nil)
}

// Only fixed labels ever leave this function, including for unknown routes.
func requestRoute(path string) string {
	switch path {
	case "/livez":
		return "livez"
	case "/readyz":
		return "readyz"
	case "/api/v1/hello":
		return "hello"
	}
	if strings.HasPrefix(path, "/api/v1/smb/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/v1/smb/"), "/")
		if len(parts) == 2 && parts[1] == "batch" {
			return "batch"
		}
		return "file"
	}
	return "other"
}
func requestMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return strings.Clone(method) // Fiber methods may borrow pooled request memory.
	}
	return "OTHER"
}
func statusOutcome(status int) string {
	if status >= 500 {
		return "server_error"
	}
	if status >= 400 {
		return "client_error"
	}
	return "complete"
}
func (o *observability) finish(id, route, method string, status int, bytes int64, outcome string, started time.Time) {
	elapsed := time.Since(started).Seconds()
	o.active.Add(-1)
	o.mu.Lock()
	o.requests[requestKey{route, method, fmt.Sprint(status), outcome}]++
	total := o.routes[route]
	if total == nil {
		total = &routeTotals{}
		o.routes[route] = total
	}
	total.count++
	total.bytes += uint64(bytes)
	total.seconds += elapsed
	for i, bound := range durationBounds {
		if elapsed <= bound {
			total.buckets[i]++
		}
	}
	o.mu.Unlock()
	// Probe successes stay in metrics without flooding the application log.
	if (route == "livez" || route == "readyz") && status == 200 && outcome == "complete" {
		return
	}
	o.logger.Info("http_request", "request_id", id, "route", route, "method", method, "status", status,
		"duration_seconds", elapsed, "response_bytes", bytes, "outcome", outcome)
}

type observedWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	failed bool
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	w.failed = w.failed || err != nil
	return n, err
}
func (w *observedWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.failed = true
	}
	return err
}

func (o *observability) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		started := time.Now()
		route, method := requestRoute(r.URL.Path), requestMethod(r.Method)
		w.Header().Set("X-Request-ID", id)
		tracked := &observedWriter{ResponseWriter: w}
		o.active.Add(1)
		defer func() {
			p := recover()
			outcome := statusOutcome(tracked.status)
			if p != nil {
				outcome = "panic"
				if p == http.ErrAbortHandler {
					outcome = "aborted"
				} else if tracked.status == 0 {
					// Do not let net/http log arbitrary panic values or stack-local secrets.
					http.Error(tracked, "internal server error", 500)
					p = nil
				}
			} else if r.Context().Err() != nil {
				outcome = "canceled"
			} else if tracked.failed {
				outcome = "aborted"
			}
			if tracked.status == 0 && p == nil {
				tracked.status = 200
			}
			o.finish(id, route, method, tracked.status, tracked.bytes, outcome, started)
			if p != nil {
				panic(http.ErrAbortHandler)
			} // Preserve late-stream abort semantics.
		}()
		next.ServeHTTP(tracked, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// Fiber's response is buffered: duration/bytes describe handler execution and
// encoded body size, not socket delivery or client disconnect detection.
func (o *observability) fiberMiddleware(c *fiber.Ctx) error {
	id := newRequestID()
	started := time.Now()
	route, method := requestRoute(c.Path()), requestMethod(c.Method())
	c.Set("X-Request-ID", id)
	c.Request().Header.Set("X-Request-ID", id)
	o.active.Add(1)
	err := c.Next()
	if err != nil {
		_ = c.App().ErrorHandler(c, err)
	}
	status := c.Response().StatusCode()
	size := len(c.Response().Body())
	if c.Method() == http.MethodHead {
		size = 0
	}
	o.finish(id, route, method, status, int64(size), statusOutcome(status), started)
	return nil
}

// Prometheus text format 0.0.4, fixed metric names and bounded label values.
// It runs on a separate opt-in listener, never the application mux/Route.
func (o *observability) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP smb_gateway_http_in_flight Requests currently executing.\n# TYPE smb_gateway_http_in_flight gauge\nsmb_gateway_http_in_flight %d\n", o.active.Load())
	fmt.Fprintf(&b, "# HELP smb_gateway_go_goroutines Current Go goroutines.\n# TYPE smb_gateway_go_goroutines gauge\nsmb_gateway_go_goroutines %d\n", runtime.NumGoroutine())
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	fmt.Fprintf(&b, "# HELP smb_gateway_go_heap_alloc_bytes Bytes of allocated Go heap, not RSS.\n# TYPE smb_gateway_go_heap_alloc_bytes gauge\nsmb_gateway_go_heap_alloc_bytes %d\n", memory.Alloc)
	if o.stats != nil {
		s := o.stats()
		fmt.Fprintf(&b, "# HELP smb_gateway_smb_active_reads Occupied SMB admission slots including cleanup.\n# TYPE smb_gateway_smb_active_reads gauge\nsmb_gateway_smb_active_reads %d\n", s.ActiveReads)
		fmt.Fprintf(&b, "# HELP smb_gateway_smb_queued_reads Calls awaiting SMB admission.\n# TYPE smb_gateway_smb_queued_reads gauge\nsmb_gateway_smb_queued_reads %d\n", s.QueuedReads)
	}
	o.mu.Lock()
	var lines []string
	for k, n := range o.requests {
		lines = append(lines, fmt.Sprintf("smb_gateway_http_requests_total{route=%q,method=%q,status=%q,outcome=%q} %d\n", k.route, k.method, k.status, k.outcome, n))
	}
	sort.Strings(lines)
	b.WriteString("# HELP smb_gateway_http_requests_total Completed requests by route and observed outcome.\n# TYPE smb_gateway_http_requests_total counter\n")
	for _, line := range lines {
		b.WriteString(line)
	}
	names := make([]string, 0, len(o.routes))
	for name := range o.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString("# HELP smb_gateway_http_response_bytes_total Body bytes accepted by the writer or buffered by Fiber, not client acknowledgements.\n# TYPE smb_gateway_http_response_bytes_total counter\n")
	for _, name := range names {
		fmt.Fprintf(&b, "smb_gateway_http_response_bytes_total{route=%q} %d\n", name, o.routes[name].bytes)
	}
	b.WriteString("# HELP smb_gateway_http_request_duration_seconds Handler duration including native streaming.\n# TYPE smb_gateway_http_request_duration_seconds histogram\n")
	for _, name := range names {
		total := o.routes[name]
		for i, bound := range durationBounds {
			fmt.Fprintf(&b, "smb_gateway_http_request_duration_seconds_bucket{route=%q,le=%q} %d\n", name, fmt.Sprint(bound), total.buckets[i])
		}
		fmt.Fprintf(&b, "smb_gateway_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\nsmb_gateway_http_request_duration_seconds_sum{route=%q} %g\nsmb_gateway_http_request_duration_seconds_count{route=%q} %d\n", name, total.count, name, total.seconds, name, total.count)
	}
	o.mu.Unlock()
	_, _ = fmt.Fprint(w, b.String())
}
