//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jasonjiang8866/smbfileserver/smbhttp"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"
	"github.com/jasonjiang8866/smbfileserver/client"
)

func integrationConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("SMB_INTEGRATION") != "1" {
		t.Skip("run scripts/test-samba.py for isolated Samba fixtures")
	}
	for _, key := range []string{"SMB_TEST_HOST", "SMB_TEST_PORT", "SMB_TEST_USER", "SMB_TEST_PASSWORD"} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s is required when SMB_INTEGRATION=1", key)
		}
	}
	return Config{SMBHost: os.Getenv("SMB_TEST_HOST"), SMBPort: os.Getenv("SMB_TEST_PORT"), SMBUser: os.Getenv("SMB_TEST_USER"), SMBPassword: os.Getenv("SMB_TEST_PASSWORD"), RequestTimeout: 5 * time.Second}
}

func TestSMBIntegrationReadAndHTTP(t *testing.T) {
	cfg := integrationConfig(t)
	reader := testSMBClient(t, cfg)
	pattern := make([]byte, 256)
	for i := range pattern {
		pattern[i] = byte(i)
	}
	cases := []struct {
		path string
		want []byte
	}{
		{"hello.txt", []byte("SMB integration fixture\n")},
		{"empty.txt", []byte{}},
		{"nested/工艺 报表.csv", []byte("lot,value\n晶圆,42\n")},
		{strings.Repeat("a", 200) + ".txt", []byte("long name\n")},
		{"large.bin", bytes.Repeat(pattern, 16384)},
	}
	for _, share := range []string{"readonly", "encrypted"} {
		t.Run(share, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.path, func(t *testing.T) {
					data, err := reader.ReadFile(context.Background(), share, tc.path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(data, tc.want) {
						t.Fatalf("content mismatch: got %d bytes, want %d", len(data), len(tc.want))
					}
				})
			}
			app := newApp(cfg, reader, context.Background())
			resp := request(t, app, "GET", "/api/v1/smb/"+share+"/hello.txt", "")
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || !bytes.Equal(data, cases[0].want) {
				t.Fatalf("HTTP read failed: status=%d", resp.StatusCode)
			}
			resp = request(t, app, "POST", "/api/v1/smb/"+share+"/batch", `{"files":[{"fileName":"hello.txt"},{"fileName":"missing.txt"},{"fileName":"nested/工艺 报表.csv"},{"fileName":"hello.txt"}],"concurrency":4}`)
			var batch smbhttp.BatchFileResponse
			if err := json.NewDecoder(resp.Body).Decode(&batch); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || len(batch.Files) != 4 {
				t.Fatal("invalid batch response")
			}
			for i, file := range batch.Files {
				if i == 1 {
					if file.Error != "file or share not found" {
						t.Fatalf("missing file: %q", file.Error)
					}
					continue
				}
				want := cases[0].want
				if i == 2 {
					want = cases[2].want
				}
				data, err := base64.StdEncoding.DecodeString(file.Content)
				if err != nil || file.Error != "" || !bytes.Equal(data, want) {
					t.Fatalf("batch item %d mismatch", i)
				}
			}
		})
	}
}

func TestSMBIntegrationErrorsAndRecovery(t *testing.T) {
	cfg := integrationConfig(t)
	reader := testSMBClient(t, cfg)
	for _, tc := range []struct {
		share, file string
		want        error
	}{
		{"readonly", "missing.txt", fs.ErrNotExist}, {"readonly", "denied.txt", fs.ErrPermission}, {"missing-share", "hello.txt", nil},
	} {
		_, err := reader.ReadFile(context.Background(), tc.share, tc.file)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Fatalf("%s/%s: err=%v", tc.share, tc.file, err)
		}
	}
	bad := cfg
	bad.SMBPassword = "invalid-synthetic-password"
	app := newApp(cfg, testSMBClient(t, bad), context.Background())
	resp := request(t, app, "GET", "/api/v1/smb/readonly/hello.txt", "")
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 502 || string(data) != `{"error":"SMB request failed"}` {
		t.Fatalf("unsafe authentication response: status=%d", resp.StatusCode)
	}
	// Same production reader can reconnect after failures; no process restart.
	if _, err := reader.ReadFile(context.Background(), "readonly", "hello.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestSMBIntegrationDirectoryAndReadOnly(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.SMBHost, cfg.SMBPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	dialer := smb2.Dialer{Negotiator: smb2.Negotiator{RequireMessageSigning: true}, Initiator: &smb2.NTLMInitiator{User: cfg.SMBUser, Password: cfg.SMBPassword}}
	session, err := dialer.DialConn(ctx, conn, cfg.SMBHost)
	if err != nil {
		t.Fatal(err)
	}
	session = session.WithContext(ctx)
	defer session.Logoff()
	share, err := session.Mount("readonly")
	if err != nil {
		t.Fatal(err)
	}
	share = share.WithContext(ctx)
	defer share.Umount()
	entries, err := share.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == "hello.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("directory listing omitted fixture")
	}
	file, err := share.Create("must-not-write.txt")
	if err == nil {
		file.Close()
		t.Fatal("read-only share permitted a write")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission denial: %v", err)
	}
}

// A loopback proxy pauses after an established SMB transfer has returned 64 KiB.
// This tests cancellation/deadlines during file I/O, not only during negotiation.
type pausedProxy struct {
	listener              net.Listener
	ready, release        chan struct{}
	gateOnce, releaseOnce sync.Once
	mu                    sync.Mutex
	connections           []net.Conn
	wg                    sync.WaitGroup
	target                string
}

func newPausedProxy(t *testing.T, target string) *pausedProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pausedProxy{listener: listener, ready: make(chan struct{}), release: make(chan struct{}), target: target}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			down, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.connections = append(p.connections, down)
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer down.Close()
				up, err := net.DialTimeout("tcp", p.target, time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				p.mu.Lock()
				p.connections = append(p.connections, up)
				p.mu.Unlock()
				copied := make(chan struct{})
				go func() { defer close(copied); _, _ = io.Copy(up, down); _ = up.Close() }()
				buffer := make([]byte, 32768)
				total := 0
				for {
					n, err := up.Read(buffer)
					if n > 0 {
						total += n
						if total >= 65536 {
							p.gateOnce.Do(func() { close(p.ready); <-p.release })
						}
						if _, writeErr := down.Write(buffer[:n]); writeErr != nil {
							break
						}
					}
					if err != nil {
						break
					}
				}
				_ = down.Close()
				_ = up.Close()
				<-copied
			}()
		}
	}()
	return p
}
func (p *pausedProxy) unpause() { p.releaseOnce.Do(func() { close(p.release) }) }
func (p *pausedProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.connections {
		_ = c.Close()
	}
}
func (p *pausedProxy) close() { _ = p.listener.Close(); p.unpause(); p.drop(); p.wg.Wait() }

func TestSMBIntegrationInterruptedRead(t *testing.T) {
	base := integrationConfig(t)
	for _, mode := range []string{"cancel", "deadline", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			proxy := newPausedProxy(t, net.JoinHostPort(base.SMBHost, base.SMBPort))
			defer proxy.close()
			cfg := base
			cfg.SMBHost, cfg.SMBPort, _ = net.SplitHostPort(proxy.listener.Addr().String())
			if mode == "deadline" {
				cfg.RequestTimeout = 2 * time.Second
			}
			reader := testSMBClient(t, cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := reader.ReadFile(ctx, "readonly", "large.bin"); result <- err }()
			select {
			case <-proxy.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("SMB file transfer did not reach pause")
			}
			switch mode {
			case "cancel":
				cancel()
			case "disconnect":
				proxy.drop()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("interrupted read unexpectedly succeeded")
				}
				want := map[string]error{"cancel": context.Canceled, "deadline": context.DeadlineExceeded}[mode]
				if want != nil && !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("SMB read did not release on interruption")
			}
			proxy.unpause()
			data, err := reader.ReadFile(context.Background(), "readonly", "hello.txt")
			if err != nil || string(data) != "SMB integration fixture\n" {
				t.Fatal(fmt.Errorf("reconnect failed: %w", err))
			}
		})
	}
}

// Verify the full net/http -> CloudSoda cancellation path against Samba while a
// real transfer is paused, then reuse the handler/reader for another request.
func TestSMBIntegrationHTTPDisconnect(t *testing.T) {
	base := integrationConfig(t)
	proxy := newPausedProxy(t, net.JoinHostPort(base.SMBHost, base.SMBPort))
	defer proxy.close()
	base.SMBHost, base.SMBPort, _ = net.SplitHostPort(proxy.listener.Addr().String())
	reader := testSMBClient(t, base)
	finished := make(chan error, 2)
	h, err := smbhttp.New(readerFunc(func(ctx context.Context, share, file string) ([]byte, error) {
		data, err := reader.ReadFile(ctx, share, file)
		finished <- err
		return data, err
	}), smbhttp.Options{RequestTimeout: base.RequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = io.WriteString(conn, "GET /readonly/large.bin HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proxy.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP transfer did not reach SMB pause")
	}
	_ = conn.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("disconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP disconnect did not release SMB read")
	}
	proxy.unpause()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/encrypted/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(data) != "SMB integration fixture\n" {
		t.Fatalf("HTTP recovery: status=%d err=%v", response.StatusCode, err)
	}
}

func TestSMBIntegrationClientLimits(t *testing.T) {
	cfg := integrationConfig(t)
	cfg.SMBMaxConcurrentReads = 1
	cfg.SMBMaxFileBytes = int64(len("SMB integration fixture\n"))
	reader := testSMBClient(t, cfg)
	// Exact boundary and empty files succeed, including on encrypted shares.
	for _, share := range []string{"readonly", "encrypted"} {
		data, err := reader.ReadFile(context.Background(), share, "hello.txt")
		if err != nil || string(data) != "SMB integration fixture\n" {
			t.Fatalf("exact boundary: %v", err)
		}
		data, err = reader.ReadFile(context.Background(), share, "empty.txt")
		if err != nil || len(data) != 0 {
			t.Fatalf("empty: %v", err)
		}
		data, err = reader.ReadFile(context.Background(), share, "large.bin")
		if data != nil || !errors.Is(err, client.ErrTooLarge) {
			t.Fatalf("oversized read: %v", err)
		}
	}
	app := newApp(cfg, reader, context.Background())
	response := request(t, app, "GET", "/api/v1/smb/readonly/large.bin", "")
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 413 || string(body) != `{"error":"file size limit exceeded"}` {
		t.Fatalf("size error HTTP contract: %d %s", response.StatusCode, body)
	}
	response = request(t, app, "POST", "/api/v1/smb/readonly/batch", `{"files":[{"fileName":"large.bin"},{"fileName":"hello.txt"}],"concurrency":2}`)
	var batch smbhttp.BatchFileResponse
	if err := json.NewDecoder(response.Body).Decode(&batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Files) != 2 || batch.Files[0].Error != "file size limit exceeded" || batch.Files[1].Error != "" {
		t.Fatalf("batch: %+v", batch)
	}
	data, err := base64.StdEncoding.DecodeString(batch.Files[1].Content)
	if err != nil || string(data) != "SMB integration fixture\n" {
		t.Fatal("slot was not released after oversize rejection")
	}
}

func TestSMBIntegrationSharedClientAdmission(t *testing.T) {
	cfg := integrationConfig(t)
	proxy := newPausedProxy(t, net.JoinHostPort(cfg.SMBHost, cfg.SMBPort))
	defer proxy.close()
	cfg.SMBHost, cfg.SMBPort, _ = net.SplitHostPort(proxy.listener.Addr().String())
	cfg.SMBMaxConcurrentReads = 1
	reader := testSMBClient(t, cfg)
	finished := make(chan error, 4)
	h, err := smbhttp.New(readerFunc(func(ctx context.Context, share, file string) ([]byte, error) {
		data, err := reader.ReadFile(ctx, share, file)
		finished <- err
		return data, err
	}), smbhttp.Options{RequestTimeout: cfg.RequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	first, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, err = io.WriteString(first, "GET /readonly/large.bin HTTP/1.1\r\nHost: localhost\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-proxy.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("first transfer did not pause")
	}
	// This independent HTTP request must wait behind the first active SMB read.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/readonly/hello.txt", nil)
	response, err := http.DefaultClient.Do(req)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting HTTP request: %v", err)
	}
	select {
	case err := <-finished:
		var op *client.OpError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &op) || op.Op != "admit" {
			t.Fatalf("queued read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued read did not cancel")
	}
	proxy.mu.Lock()
	connections := len(proxy.connections)
	proxy.mu.Unlock()
	if connections != 2 {
		t.Fatalf("concurrency cap bypassed: %d proxy connection ends", connections)
	}
	_ = first.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active slot not released")
	}
	proxy.unpause()
	response, err = (&http.Client{Timeout: 5 * time.Second}).Get(server.URL + "/readonly/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(data) != "SMB integration fixture\n" {
		t.Fatalf("recovery: %v", err)
	}
}
