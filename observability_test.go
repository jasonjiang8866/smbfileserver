package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jasonjiang8866/smbfileserver/client"
)

func TestRequestObservationNativeAndFiber(t *testing.T) {
	for _, mode := range []string{"native", "fiber"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := testConfig()
			cfg.observation = newObservability(slog.New(slog.NewJSONHandler(&logs, nil)), nil)
			reader := observationBackend{gatewayBackend: new(gatewayBackend), t: t}
			policy, err := loadAccessPolicy(writeTestPolicy(t), reader)
			if err != nil {
				t.Fatal(err)
			}
			h, err := newHTTPHandler(cfg, policy)
			if err != nil {
				t.Fatal(err)
			}
			app := newApp(cfg, policy, context.Background())
			ids := map[string]bool{}
			for _, tc := range []struct {
				path, token string
				status      int
			}{
				{"/api/v1/smb/reports/PRIVATE_PATH?secret=PRIVATE_QUERY", gatewayTestToken, 200},
				{"/api/v1/smb/reports/PRIVATE_PATH", "PRIVATE_TOKEN", 401},
				{"/api/v1/smb/PRIVATE_SHARE/PRIVATE_PATH", gatewayTestToken, 403},
				{"/PRIVATE_UNKNOWN?secret=PRIVATE_QUERY", "", 404},
			} {
				r := httptest.NewRequest("GET", tc.path, nil)
				r.Header.Set("Authorization", "Bearer "+tc.token)
				r.Header.Set("X-Request-ID", "PRIVATE_INCOMING_ID")
				var resp *http.Response
				if mode == "fiber" {
					resp, err = app.Test(r, 2000)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					resp = w.Result()
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				id := resp.Header.Get("X-Request-ID")
				if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) || ids[id] {
					t.Fatalf("invalid or duplicate ID %q", id)
				}
				ids[id] = true
				if resp.StatusCode != tc.status {
					t.Fatalf("status %d body %q", resp.StatusCode, body)
				}
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 4 {
				t.Fatalf("expected one completion per request, got %d", len(lines))
			}
			for _, line := range lines {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if !ids[entry["request_id"].(string)] {
					t.Fatal("response and log ID differ")
				}
			}
			w := httptest.NewRecorder()
			cfg.observation.metrics(w, httptest.NewRequest("GET", "/metrics", nil))
			for _, forbidden := range []string{"PRIVATE_", gatewayTestToken} {
				if strings.Contains(logs.String()+w.Body.String(), forbidden) {
					t.Fatal("sensitive data leaked")
				}
			}
			if !strings.Contains(w.Body.String(), `route="file",method="GET",status="401",outcome="client_error"} 1`) {
				t.Fatal(w.Body.String())
			}
			if cfg.observation.active.Load() != 0 {
				t.Fatal("active request leaked")
			}
		})
	}
}

func TestObservationPreservesStreamAbortAndFlush(t *testing.T) {
	var logs bytes.Buffer
	o := newObservability(slog.New(slog.NewJSONHandler(&logs, nil)), nil)
	flushed := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(o.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		close(flushed)
		<-release
		panic(http.ErrAbortHandler)
	})))
	defer server.Close()
	resp, err := http.Get(server.URL + "/api/v1/smb/reports/PRIVATE_PATH")
	if err != nil {
		t.Fatal(err)
	}
	<-flushed
	if o.active.Load() != 1 {
		t.Fatal("stream no longer counted while active")
	}
	first := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "first" {
		t.Fatal("early streaming lost")
	}
	close(release)
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("late failure became successful EOF")
	}
	server.Close() // Wait for the handler before reading the non-concurrent log buffer.
	if !strings.Contains(logs.String(), `"outcome":"aborted"`) || !strings.Contains(logs.String(), `"status":200`) {
		t.Fatal(logs.String())
	}
}

func TestObservationConcurrentCardinalityAndStats(t *testing.T) {
	o := newObservability(slog.New(slog.NewJSONHandler(io.Discard, nil)), func() client.Stats { return client.Stats{ActiveReads: 2, QueuedReads: 3} })
	h := o.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PRIVATE_METHOD", "/"+newRequestID(), nil))
			o.metrics(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
		}()
	}
	wg.Wait()
	if len(o.requests) != 1 || o.active.Load() != 0 {
		t.Fatal("unbounded labels or leaked active requests")
	}
	w := httptest.NewRecorder()
	o.metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, expected := range []string{"smb_gateway_smb_active_reads 2", "smb_gateway_smb_queued_reads 3", `smb_gateway_http_request_duration_seconds_count{route="other"} 50`, `smb_gateway_http_request_duration_seconds_bucket{route="other",le="+Inf"} 50`} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatal("missing metric", expected)
		}
	}
}

func TestMetricsListenerBindFailureAndConfig(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := startMetrics(listener.Addr().String(), newObservability(slog.Default(), nil)); err == nil {
		t.Fatal("bind conflict accepted")
	}
	for _, value := range []string{":0", ":invalid", "invalid"} {
		env := validEnv()
		env["METRICS_ADDR"] = value
		if _, err := loadConfig("/nonexistent", envLookup(env)); err == nil {
			t.Fatal("invalid metrics address accepted")
		}
	}
}

// Use the same context check in streaming and buffered backend entry points.
type observationBackend struct {
	*gatewayBackend
	t *testing.T
}

func (b observationBackend) Open(ctx context.Context, share, name string) (fs.File, error) {
	if id, _ := ctx.Value(requestIDKey{}).(string); len(id) != 32 {
		b.t.Error("missing downstream request ID")
	}
	return b.gatewayBackend.Open(ctx, share, name)
}
func (b observationBackend) ReadFile(ctx context.Context, share, name string) ([]byte, error) {
	f, err := b.Open(ctx, share, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func TestObservationRedactsUnexpectedPanic(t *testing.T) {
	var logs bytes.Buffer
	o := newObservability(slog.New(slog.NewJSONHandler(&logs, nil)), nil)
	w := httptest.NewRecorder()
	o.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("PRIVATE_PANIC") })).ServeHTTP(w, httptest.NewRequest("GET", "/private", nil))
	if w.Code != 500 || strings.Contains(logs.String()+w.Body.String(), "PRIVATE_PANIC") || !strings.Contains(logs.String(), `"outcome":"panic"`) {
		t.Fatal("panic not sanitized")
	}
}

func TestFiberRequestIDSurvivesRequestReuse(t *testing.T) {
	cfg := testConfig()
	cfg.observation = newObservability(slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	var contexts []context.Context
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		contexts = append(contexts, ctx)
		return []byte("safe"), nil
	})
	app := newApp(cfg, reader, context.Background())
	var ids []string
	for range 20 {
		resp, err := app.Test(httptest.NewRequest("GET", "/api/v1/smb/reports/file", nil), 1000)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strings.Clone(resp.Header.Get("X-Request-ID")))
		resp.Body.Close()
	}
	for i, ctx := range contexts {
		if ctx.Value(requestIDKey{}) != ids[i] {
			t.Fatal("request ID reused pooled memory")
		}
	}
}
