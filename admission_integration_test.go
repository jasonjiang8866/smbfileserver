//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSMBIntegrationOverloadAndRecovery(t *testing.T) {
	cfg := integrationConfig(t)
	cfg.RequestTimeout = 10 * time.Second
	cfg.SMBMaxConcurrentReads = 1
	cfg.SMBMaxQueuedReads = 1
	cfg.SMBQueueTimeout = 5 * time.Second
	reader := testSMBClient(t, cfg)
	held, err := reader.Open(context.Background(), "readonly", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h, err := newHTTPHandler(cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	httpClient := &http.Client{Timeout: 8 * time.Second}
	type result struct {
		status int
		data   []byte
		err    error
	}
	results := make(chan result, 2)
	fetch := func() {
		response, err := httpClient.Get(server.URL + "/api/v1/smb/readonly/hello.txt")
		if err != nil {
			results <- result{err: err}
			return
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		results <- result{response.StatusCode, data, err}
	}
	// One request must wait behind the open stream; the other must reject
	// immediately because there is exactly one waiting place. Ordering is free.
	go fetch()
	go fetch()
	select {
	case got := <-results:
		var body map[string]string
		if got.err != nil || got.status != 503 || json.Unmarshal(got.data, &body) != nil || body["error"] != "SMB reader busy" {
			t.Fatalf("overload: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full queue did not reject before queue timeout")
	}
	// Early Close hands the slot to the admitted waiter, with no client rebuild.
	held.Close()
	select {
	case got := <-results:
		if got.err != nil || got.status != 200 || string(got.data) != "SMB integration fixture\n" {
			t.Fatalf("waiting read: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting read did not recover")
	}
	fetch()
	got := <-results
	if got.err != nil || got.status != 200 {
		t.Fatalf("recovery: %+v", got)
	}
}

func TestSMBIntegrationQueueTimeoutHTTP(t *testing.T) {
	cfg := integrationConfig(t)
	cfg.SMBMaxConcurrentReads = 1
	cfg.SMBQueueTimeout = 50 * time.Millisecond
	reader := testSMBClient(t, cfg)
	held, err := reader.Open(context.Background(), "readonly", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	h, err := newHTTPHandler(cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/smb/readonly/hello.txt", nil))
	var body map[string]string
	if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["error"] != "SMB reader busy" {
		t.Fatalf("queue timeout: %v", w)
	}
	// Queue timeout must neither expire nor release the existing live stream.
	data, err := io.ReadAll(held)
	if err != nil || string(data) != "SMB integration fixture\n" {
		t.Fatalf("held stream: %q %v", data, err)
	}
	data, err = reader.ReadFile(context.Background(), "readonly", "hello.txt")
	if err != nil || string(data) != "SMB integration fixture\n" {
		t.Fatalf("recovery: %q %v", data, err)
	}
}
