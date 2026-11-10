package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSMBStalledHandshakeHonorsDeadlineAndClosesConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		// Consume negotiation without replying; observe the client's transport close.
		_, err = io.Copy(io.Discard, conn)
		closed <- err
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	cfg := Config{SMBHost: host, SMBPort: port, SMBUser: "reader", SMBPassword: "test", RequestTimeout: 100 * time.Millisecond}
	start := time.Now()
	_, err = (&SMBReader{config: cfg}).ReadFile(context.Background(), "images", "file")
	var timeout net.Error
	if !(errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())) || time.Since(start) > 2*time.Second {
		t.Fatalf("handshake was not bounded: %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("connection did not close cleanly: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("connection leaked")
	}
}

func TestSMBCanceledContextDoesNotDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &SMBReader{config: Config{SMBHost: "127.0.0.1", SMBPort: "1", RequestTimeout: time.Second}}
	_, err := reader.ReadFile(ctx, "images", "file")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

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
