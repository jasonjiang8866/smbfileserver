package client_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonjiang8866/smbfileserver/client"
)

func config() client.Config {
	return client.Config{Host: "files.example.test", Username: "reader", Password: "secret"}
}
func newClient(t *testing.T, cfg client.Config) *client.Client {
	t.Helper()
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*client.Config)
	}{
		{"empty host", func(c *client.Config) { c.Host = "" }}, {"UNC host", func(c *client.Config) { c.Host = `\\host\share` }},
		{"URL host", func(c *client.Config) { c.Host = "smb://host" }}, {"port in host", func(c *client.Config) { c.Host = "host:445" }},
		{"port", func(c *client.Config) { c.Port = 65536 }}, {"negative port", func(c *client.Config) { c.Port = -1 }},
		{"username", func(c *client.Config) { c.Username = " " }}, {"password", func(c *client.Config) { c.Password = "" }},
		{"timeout", func(c *client.Config) { c.Timeout = -1 }}, {"concurrency", func(c *client.Config) { c.MaxConcurrentReads = -1 }},
		{"queue count", func(c *client.Config) { c.MaxQueuedReads = -1 }}, {"queue timeout", func(c *client.Config) { c.QueueTimeout = -1 }},
		{"size", func(c *client.Config) { c.MaxFileBytes = -1 }}, {"overflow", func(c *client.Config) { c.MaxFileBytes = math.MaxInt64 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config()
			tc.change(&cfg)
			_, err := client.New(cfg)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("bad validation: %v", err)
			}
		})
	}
	for _, host := range []string{"files.example.test", "127.0.0.1", "::1"} {
		cfg := config()
		cfg.Host = host
		_ = newClient(t, cfg)
	}
}
func TestCanceledAndInvalidRequestsDoNotDial(t *testing.T) {
	server := newStalledServer(t)
	cfg := server.config()
	c := newClient(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ReadFile(ctx, "share", "file"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	for _, names := range [][2]string{{"", "file"}, {`\\host\share`, "file"}, {"share", ""}, {"share", "bad\x00name"}} {
		if _, err := c.ReadFile(context.Background(), names[0], names[1]); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid input: %v", err)
		}
	}
	if server.calls.Load() != 0 {
		t.Fatal("invalid or canceled request dialed server")
	}
}

type stalledServer struct {
	listener         net.Listener
	calls            atomic.Int32
	accepted, closed chan struct{}
	wg               sync.WaitGroup
	mu               sync.Mutex
	conns            []net.Conn
}

func newStalledServer(t *testing.T) *stalledServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stalledServer{listener: l, accepted: make(chan struct{}, 10), closed: make(chan struct{}, 10)}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			s.calls.Add(1)
			s.accepted <- struct{}{}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(io.Discard, c)
				s.closed <- struct{}{}
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}
func (s *stalledServer) config() client.Config {
	cfg := config()
	host, p, _ := net.SplitHostPort(s.listener.Addr().String())
	cfg.Host = host
	cfg.Port, _ = strconv.Atoi(p)
	cfg.Timeout = 3 * time.Second
	cfg.MaxConcurrentReads = 1
	return cfg
}
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(what)
	}
}
func result(t *testing.T, ch <-chan error, want error) {
	t.Helper()
	select {
	case err := <-ch:
		if !errors.Is(err, want) {
			t.Fatalf("got %v want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not terminate")
	}
}

func TestAdmissionCancellationAndSlotReuse(t *testing.T) {
	server := newStalledServer(t)
	cfg := server.config()
	c := newClient(t, cfg)
	// Configuration is snapshotted; changing caller-owned fields must not redirect it.
	cfg.Host = "invalid.invalid"
	cfg.Password = "changed"
	active, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.ReadFile(active, "share", "file"); done <- err }()
	wait(t, server.accepted, "first read did not connect")
	// The only active slot is occupied by a stalled negotiation.
	ctx, stop := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop()
	_, err := c.ReadFile(ctx, "share", "queued")
	var op *client.OpError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &op) || op.Op != "admit" {
		t.Fatalf("queued read: %v", err)
	}
	if server.calls.Load() != 1 {
		t.Fatal("queued timeout opened another connection")
	}
	cancel()
	result(t, done, context.Canceled)
	wait(t, server.closed, "canceled transport stayed open")
	next, finish := context.WithCancel(context.Background())
	defer finish()
	go func() { _, err := c.ReadFile(next, "share", "again"); done <- err }()
	wait(t, server.accepted, "released slot not reusable")
	finish()
	result(t, done, context.Canceled)
	wait(t, server.closed, "second transport stayed open")
}
func TestStalledHandshakeHonorsClientTimeout(t *testing.T) {
	server := newStalledServer(t)
	cfg := server.config()
	cfg.Timeout = 70 * time.Millisecond
	c := newClient(t, cfg)
	started := time.Now()
	_, err := c.ReadFile(context.Background(), "share", "file")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("deadline: %v", err)
	}
	wait(t, server.closed, "timed-out transport stayed open")
}
func TestConnectionFailureIsBackendNeutral(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, p, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	cfg := config()
	cfg.Host = host
	cfg.Port, _ = strconv.Atoi(p)
	c := newClient(t, cfg)
	data, err := c.ReadFile(context.Background(), "private-share", "private-file")
	var op *client.OpError
	if data != nil || !errors.Is(err, client.ErrUnavailable) || !errors.As(err, &op) || op.Op != "connect" {
		t.Fatalf("unexpected failure: %v", err)
	}
	for _, secret := range []string{cfg.Password, host, "private-share", "private-file"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("diagnostic exposed input")
		}
	}
}

func TestQueueTimeoutDoesNotDialOrCancelActiveRead(t *testing.T) {
	server := newStalledServer(t)
	cfg := server.config()
	cfg.QueueTimeout = 30 * time.Millisecond
	c := newClient(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Open(ctx, "share", "file"); done <- err }()
	wait(t, server.accepted, "active read did not connect")
	for range 2 {
		_, err := c.Open(context.Background(), "share", "queued")
		var op *client.OpError
		if !errors.Is(err, client.ErrBusy) || !errors.As(err, &op) || op.Op != "admit" {
			t.Fatalf("queue timeout: %v", err)
		}
	}
	if server.calls.Load() != 1 {
		t.Fatal("rejected waiters opened connections")
	}
	select {
	case err := <-done:
		t.Fatalf("queue timeout canceled active call: %v", err)
	default:
	}
	cancel()
	result(t, done, context.Canceled)
	wait(t, server.closed, "active transport stayed open")
}
