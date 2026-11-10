//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"
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
	reader := &SMBReader{config: cfg}
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
			var batch BatchFileResponse
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
	reader := &SMBReader{config: cfg}
	for _, tc := range []struct {
		share, file string
		status      int
	}{
		{"readonly", "missing.txt", 404}, {"readonly", "denied.txt", 403}, {"missing-share", "hello.txt", 502},
	} {
		_, err := reader.ReadFile(context.Background(), tc.share, tc.file)
		status, _ := publicReadError(err)
		if err == nil || status != tc.status {
			t.Fatalf("%s/%s: status=%d err=%v", tc.share, tc.file, status, err)
		}
	}
	bad := cfg
	bad.SMBPassword = "invalid-synthetic-password"
	app := newApp(cfg, &SMBReader{config: bad}, context.Background())
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
			reader := &SMBReader{config: cfg}
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
				status, _ := publicReadError(err)
				want := map[string]int{"cancel": 503, "deadline": 504, "disconnect": 502}[mode]
				if status != want {
					t.Fatalf("got status=%d, want=%d: %v", status, want, err)
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
