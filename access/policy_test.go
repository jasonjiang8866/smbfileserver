package access_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/jasonjiang8866/smbfileserver/access"
	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

const tokenA = "test-only-reader-token-aaaaaaaaaaaaaaa"
const tokenB = "test-only-other-token-bbbbbbbbbbbbbbbb"
const tokenNew = "test-only-rotated-token-cccccccccccccc"

func hash(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }
func config() access.Config {
	return access.Config{Shares: []access.Share{{ID: "reports", Name: "private-share"}, {ID: "other", Name: "second-share"}}, Services: []access.Service{{ID: "reader", TokenSHA256: []string{hash(tokenA), hash(tokenNew)}, Shares: []string{"reports"}}, {ID: "other-reader", TokenSHA256: []string{hash(tokenB)}, Shares: []string{"other"}}}}
}

type backend struct{ calls atomic.Int32 }

func (b *backend) Open(_ context.Context, share, name string) (fs.File, error) {
	b.calls.Add(1)
	if share != "private-share" && share != "second-share" {
		return nil, fmt.Errorf("unmapped share")
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: []byte("fixture"), Mode: 0444}}.Open(name)
}
func (b *backend) ReadFile(ctx context.Context, share, name string) ([]byte, error) {
	f, err := b.Open(ctx, share, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return []byte("fixture"), nil
}
func policy(t *testing.T, cfg access.Config, b *backend) *access.Policy {
	t.Helper()
	p, err := access.New(cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func handler(t *testing.T, p *access.Policy, buffered bool) http.Handler {
	t.Helper()
	h, err := smbhttp.New(p, smbhttp.Options{DisableStreaming: buffered})
	if err != nil {
		t.Fatal(err)
	}
	return p.Wrap(h)
}
func call(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthenticationBeforeRoutingAndBackend(t *testing.T) {
	b := new(backend)
	p := policy(t, config(), b)
	h := handler(t, p, false)
	for _, value := range []string{"", "bad", strings.Repeat("x", 32), tokenA + " ", tokenA + ",other"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			w := call(h, method, "/reports/batch", "malformed JSON", value)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("%s: %v", method, w)
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD body")
			}
			if strings.Contains(w.Body.String(), tokenA) {
				t.Fatal("token leaked")
			}
		}
	}
	for _, headers := range [][]string{{"Bearer " + tokenA, "Bearer " + tokenA}, {"Basic " + tokenA}, {"Bearer  " + tokenA}} {
		r := httptest.NewRequest("GET", "/reports/file?token="+tokenA, nil)
		r.Header.Set("Cookie", "token="+tokenA)
		r.Header.Set("X-Forwarded-User", "reader")
		for _, value := range headers {
			r.Header.Add("Authorization", value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	if b.calls.Load() != 0 {
		t.Fatal("unauthenticated request called backend")
	}
}
func TestShareAuthorizationAcrossReadModesAndBatch(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		b := new(backend)
		p := policy(t, config(), b)
		h := handler(t, p, buffered)
		for _, name := range []string{"other", "private-share", "unknown", "REPORTS"} {
			for _, method := range []string{"GET", "HEAD"} {
				if w := call(h, method, "/"+name+"/file", "", tokenA); w.Code != 403 {
					t.Fatalf("%s: %v", name, w)
				}
			}
			w := call(h, "POST", "/"+name+"/batch", `{"files":[{"fileName":"a"},{"fileName":"b"}]}`, tokenA)
			var out smbhttp.BatchFileResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Files) != 2 {
				t.Fatal(w)
			}
			for _, f := range out.Files {
				if f.Error != "file access denied" {
					t.Fatal(f)
				}
			}
		}
		if b.calls.Load() != 0 {
			t.Fatal("unauthorized backend access")
		}
		for _, token := range []string{tokenA, tokenNew} {
			w := call(h, "GET", "/reports/file", "", token)
			if w.Code != 200 || w.Body.String() != "fixture" || w.Header().Get("Content-Disposition") != "attachment" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w)
			}
		}
		if w := call(h, "GET", "/other/file", "", tokenB); w.Code != 200 {
			t.Fatal(w)
		}
	}
}
func TestAllBatchPathsAreCheckedAndUnicodeWorks(t *testing.T) {
	b := new(backend)
	h := handler(t, policy(t, config(), b), false)
	bad := []string{"../secret", "a/../secret", "/etc/passwd", `..\secret`, `a\file`, `\\server\share\file`, `C:\file`, `C:file`, `file:stream`, "%2e%2e/secret", "%252e%252e/secret", "a//b", "a/./b", "a/", "a./file", "a /file", "a\x00b", "a\nb", "CON", "nul.txt", "COM1.txt", "LPT¹.txt", "file?", "file*", strings.Repeat("x", 4097)}
	for _, name := range bad {
		body, _ := json.Marshal(smbhttp.BatchFileRequest{Files: []smbhttp.FileRequest{{FileName: name}}})
		w := call(h, "POST", "/reports/batch", string(body), tokenA)
		var out smbhttp.BatchFileResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Files) != 1 || out.Files[0].Error != "invalid file or share name" {
			t.Fatalf("%q: %v", name, w)
		}
	}
	for _, path := range []string{"/reports/%252e%252e", "/reports/..%5csecret", "/reports/file%3Astream", "/reports/%00", "/reports/a%2fb"} {
		w := call(h, "GET", path, "", tokenA)
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("encoded path %q status %d", path, w.Code)
		}
	}
	if b.calls.Load() != 0 {
		t.Fatal("invalid paths called backend")
	}
	w := call(h, "POST", "/reports/batch", `{"files":[{"fileName":"nested/工艺 报表.csv"},{"fileName":"../denied"},{"fileName":"hello.txt"}]}`, tokenA)
	var out smbhttp.BatchFileResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Files) != 3 || out.Files[0].Error != "" || out.Files[1].Error != "invalid file or share name" || out.Files[2].Error != "" || b.calls.Load() != 2 {
		t.Fatal(w)
	}
}
func TestPolicyIdentityIsolationSnapshotAndRotation(t *testing.T) {
	b := new(backend)
	cfg := config()
	p := policy(t, cfg, b)
	cfg.Shares[0].Name = "changed"
	cfg.Services[0].Shares[0] = "other"
	cfg.Services[0].TokenSHA256[0] = hash("changed")
	var captured context.Context
	capture := p.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { captured = r.Context() }))
	if call(capture, "GET", "/", "", tokenA).Code != 200 {
		t.Fatal("config not snapshotted")
	}
	if _, err := p.Open(context.Background(), "reports", "file"); !errors.Is(err, fs.ErrPermission) {
		t.Fatal(err)
	}
	if _, err := p.ReadFile(captured, "reports", "file"); err != nil {
		t.Fatal(err)
	}
	rotated := config()
	rotated.Services[0].TokenSHA256 = []string{hash(tokenNew)}
	p2 := policy(t, rotated, b)
	if _, err := p2.Open(captured, "reports", "file"); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("identity crossed policy boundary")
	}
	if w := call(handler(t, p2, false), "GET", "/reports/file", "", tokenA); w.Code != 401 {
		t.Fatal("old token not revoked")
	}
	if w := call(handler(t, p2, false), "GET", "/reports/file", "", tokenNew); w.Code != 200 {
		t.Fatal("replacement token failed")
	}
}
func TestInvalidPoliciesFailClosedAndSanitized(t *testing.T) {
	for _, change := range []func(*access.Config){
		func(c *access.Config) { c.Shares = nil }, func(c *access.Config) { c.Services = nil },
		func(c *access.Config) { c.Shares[0].Name = `\\host\secret` }, func(c *access.Config) { c.Shares[0].ID = "*" },
		func(c *access.Config) { c.Shares[1].ID = "reports" }, func(c *access.Config) { c.Services[1].ID = "reader" },
		func(c *access.Config) { c.Services[0].Shares = []string{"unknown"} }, func(c *access.Config) { c.Services[0].Shares = nil },
		func(c *access.Config) { c.Services[0].TokenSHA256 = []string{"secret-do-not-echo"} },
		func(c *access.Config) { c.Services[1].TokenSHA256 = []string{hash(tokenA)} },
	} {
		cfg := config()
		change(&cfg)
		_, err := access.New(cfg, new(backend))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe validation %v", err)
		}
	}
}
