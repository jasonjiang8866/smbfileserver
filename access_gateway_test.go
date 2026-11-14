package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jasonjiang8866/smbfileserver/access"
)

const gatewayTestToken = "synthetic-gateway-token-do-not-use-in-production"

func testAccessConfig() access.Config {
	digest := sha256.Sum256([]byte(gatewayTestToken))
	return access.Config{Shares: []access.Share{{ID: "reports", Name: "readonly"}, {ID: "blocked", Name: "encrypted"}}, Services: []access.Service{{ID: "reader", TokenSHA256: []string{hex.EncodeToString(digest[:])}, Shares: []string{"reports"}}}}
}
func writeTestPolicy(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(testAccessConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "access.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type gatewayBackend struct{ calls atomic.Int32 }

func (b *gatewayBackend) Open(_ context.Context, share, name string) (fs.File, error) {
	b.calls.Add(1)
	if share != "readonly" {
		return nil, fs.ErrPermission
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: []byte("safe")}}.Open(name)
}
func (b *gatewayBackend) ReadFile(ctx context.Context, share, name string) ([]byte, error) {
	f, err := b.Open(ctx, share, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func TestGatewayAccessNativeAndFiber(t *testing.T) {
	for _, mode := range []string{"native", "fiber"} {
		t.Run(mode, func(t *testing.T) {
			b := new(gatewayBackend)
			p, err := loadAccessPolicy(writeTestPolicy(t), b)
			if err != nil {
				t.Fatal(err)
			}
			cfg := testConfig()
			h, err := newHTTPHandler(cfg, p)
			if err != nil {
				t.Fatal(err)
			}
			app := newApp(cfg, p, context.Background())
			invoke := func(r *http.Request) *http.Response {
				t.Helper()
				if mode == "fiber" {
					resp, err := app.Test(r, 5000)
					if err != nil {
						t.Fatal(err)
					}
					return resp
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Result()
			}
			for _, tc := range []struct {
				path, body, token string
				want              int
			}{
				{"/api/v1/smb/reports/file", "", "", 401},
				{"/api/v1/smb/reports/file", "", gatewayTestToken, 200},
				{"/api/v1/smb/readonly/file", "", gatewayTestToken, 403},
				{"/api/v1/smb/blocked/file", "", gatewayTestToken, 403},
				{"/api/v1/smb/reports/..%5csecret", "", gatewayTestToken, 400},
				{"/api/v1/smb/reports/batch", `{"files":[{"fileName":"../secret"}]}`, gatewayTestToken, 200},
			} {
				method := "GET"
				if tc.body != "" {
					method = "POST"
				}
				r := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/json")
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				resp := invoke(r)
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != tc.want {
					t.Fatalf("%s status=%d body=%s", tc.path, resp.StatusCode, data)
				}
				if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Disposition") != "attachment" {
					t.Fatal("missing response policy")
				}
				if tc.body != "" && !strings.Contains(string(data), "invalid file or share name") {
					t.Fatal(string(data))
				}
			}
			// Adapters must not collapse duplicate credentials into an accepted token.
			r := httptest.NewRequest("GET", "/api/v1/smb/reports/file", nil)
			r.Header.Add("Authorization", "Bearer "+gatewayTestToken)
			r.Header.Add("Authorization", "Bearer "+gatewayTestToken)
			resp := invoke(r)
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatal("duplicate authorization accepted")
			}
			if b.calls.Load() != 1 {
				t.Fatalf("denied requests reached SMB: %d calls", b.calls.Load())
			}
		})
	}
}
func TestAccessConfigErrorsAreClosedAndSanitized(t *testing.T) {
	b := new(gatewayBackend)
	if _, err := loadAccessPolicy(filepath.Join(t.TempDir(), "missing"), b); err == nil {
		t.Fatal("missing policy accepted")
	}
	for _, data := range []string{"", `{"secret":"secret-do-not-echo"}`, `{"shares":[],"services":[]}`, `{} {}`, `null`, strings.Repeat("x", (1<<20)+1)} {
		path := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAccessPolicy(path, b); err == nil || strings.Contains(err.Error(), "secret-do-not-echo") {
			t.Fatalf("unsafe config error %v", err)
		}
	}
	// An unsupported subdirectory root must not be silently ignored.
	data, _ := json.Marshal(testAccessConfig())
	data = []byte(strings.Replace(string(data), `"name":"readonly"`, `"name":"readonly","root":"nested"`, 1))
	path := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(path, data, 0600)
	if _, err := loadAccessPolicy(path, b); err == nil {
		t.Fatal("subdirectory mapping silently accepted")
	}
}
func TestExecutableRejectsPolicyBeforeListening(t *testing.T) {
	for key, value := range validEnv() {
		t.Setenv(key, value)
	}
	t.Setenv("ACCESS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	if err := run(); err == nil || err.Error() != "cannot open ACCESS_CONFIG_FILE" {
		t.Fatalf("startup did not fail closed: %v", err)
	}
}
