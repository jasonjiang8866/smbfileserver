package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGatewayProbesDoNotAccessSMB(t *testing.T) {
	for _, mode := range []string{"native", "fiber"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig()
			cfg.health = &gatewayHealth{}
			reader := readerFunc(func(context.Context, string, string) ([]byte, error) { t.Error("probe called SMB"); return nil, nil })
			h, err := newHTTPHandler(cfg, reader)
			if err != nil {
				t.Fatal(err)
			}
			app := newApp(cfg, reader, context.Background())
			for _, draining := range []bool{false, true} {
				if draining {
					cfg.health.drain()
				}
				for _, path := range []string{"/livez", "/readyz"} {
					for _, method := range []string{"GET", "HEAD", "POST"} {
						r := httptest.NewRequest(method, path, nil)
						var resp *http.Response
						if mode == "fiber" {
							resp, err = app.Test(r, 1000)
							if err != nil {
								t.Fatal(err)
							}
						} else {
							w := httptest.NewRecorder()
							h.ServeHTTP(w, r)
							resp = w.Result()
						}
						want := 200
						if draining && path == "/readyz" {
							want = 503
						}
						if method == "POST" {
							want = 405
						}
						if resp.StatusCode != want || resp.Header.Get("Cache-Control") != "no-store" {
							t.Fatalf("%s %s drain=%v: %d", method, path, draining, resp.StatusCode)
						}
						resp.Body.Close()
					}
				}
			}
		})
	}
}

func TestShutdownMarksDraining(t *testing.T) {
	for _, mode := range []string{"native", "fiber"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			requests, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := testConfig()
			cfg.health = &gatewayHealth{}
			reader := readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, nil })
			done := make(chan error, 1)
			drained := make(chan struct{})
			onDrain := func() { cfg.health.drain(); close(drained) }
			if mode == "fiber" {
				app := newApp(cfg, reader, requests)
				go func() { done <- serve(ctx, app, listener, time.Second, cancel, onDrain) }()
			} else {
				h, err := newHTTPHandler(cfg, reader)
				if err != nil {
					t.Fatal(err)
				}
				go func() { done <- serveHTTP(ctx, &http.Server{Handler: h}, listener, time.Second, cancel, onDrain) }()
			}
			stop()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
				t.Fatal("drain not signaled")
			}
			if !cfg.health.draining.Load() {
				t.Fatal("readiness remained true")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown stuck")
			}
		})
	}
}
