package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeImmediateCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := newApp(testConfig(), readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, nil }), root)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, app, listener, time.Second, cancel) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown raced listener startup")
	}
}

func TestServeDrainsInflightRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		select {
		case <-release:
			return []byte("completed"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	app := newApp(testConfig(), reader, root)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, app, listener, time.Second, cancel) }()
	response := make(chan string, 1)
	go func() {
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String() + "/api/v1/smb/images/file")
		if err != nil {
			response <- err.Error()
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		response <- string(data)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	stop()
	// Graceful shutdown must not cancel the file operation immediately.
	if root.Err() != nil {
		t.Fatal("in-flight work canceled before grace period")
	}
	close(release)
	select {
	case body := <-response:
		if body != "completed" {
			t.Fatalf("request interrupted: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request stuck")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestServeCancelsWorkAtGraceDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})
	app := newApp(testConfig(), reader, root)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, app, listener, 30*time.Millisecond, cancel) }()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String() + "/api/v1/smb/images/file")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	stop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "grace period") {
			t.Fatalf("expected timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stuck")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("SMB work was not canceled")
	}
	select {
	case <-clientDone:
	case <-time.After(4 * time.Second):
		t.Fatal("client did not exit")
	}
}

func TestNativeServeImmediateCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, err := newHTTPHandler(testConfig(), readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, nil }))
	done := make(chan error, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return root }}
	go func() { done <- serveHTTP(ctx, server, listener, time.Second, cancel) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown raced listener startup")
	}
}

func TestNativeServeDrainsInflightRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		select {
		case <-release:
			return []byte("completed"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	handler, err := newHTTPHandler(testConfig(), reader)
	done := make(chan error, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return root }}
	go func() { done <- serveHTTP(ctx, server, listener, time.Second, cancel) }()
	response := make(chan string, 1)
	go func() {
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String() + "/api/v1/smb/images/file")
		if err != nil {
			response <- err.Error()
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		response <- string(data)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	stop()
	// Graceful shutdown must not cancel the file operation immediately.
	if root.Err() != nil {
		t.Fatal("in-flight work canceled before grace period")
	}
	close(release)
	select {
	case body := <-response:
		if body != "completed" {
			t.Fatalf("request interrupted: %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request stuck")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestNativeServeCancelsWorkAtGraceDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, finished := make(chan struct{}), make(chan struct{})
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})
	handler, err := newHTTPHandler(testConfig(), reader)
	done := make(chan error, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return root }}
	go func() { done <- serveHTTP(ctx, server, listener, 30*time.Millisecond, cancel) }()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String() + "/api/v1/smb/images/file")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	stop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "grace period") {
			t.Fatalf("expected timeout: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stuck")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("SMB work was not canceled")
	}
	select {
	case <-clientDone:
	case <-time.After(4 * time.Second):
		t.Fatal("client did not exit")
	}
}
