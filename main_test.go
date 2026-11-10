package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

type readerFunc func(context.Context, string, string) ([]byte, error)

func (f readerFunc) ReadFile(ctx context.Context, share, file string) ([]byte, error) {
	return f(ctx, share, file)
}
func testConfig() Config { return Config{RequestTimeout: time.Second, ShutdownTimeout: time.Second} }
func request(t *testing.T, app *fiber.App, method, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
func TestProductionBatchRoutes(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			reader := readerFunc(func(ctx context.Context, share, file string) ([]byte, error) {
				calls.Add(1)
				if share != "images" || file == "batch" {
					return nil, errors.New("wrong route")
				}
				if file == "missing.png" {
					return nil, fmt.Errorf("private server path: %w", fs.ErrNotExist)
				}
				return []byte(file), nil
			})
			app := newApp(testConfig(), reader, context.Background())
			resp := request(t, app, method, "/api/v1/smb/images/batch?files=one.png,missing.png,two.png", `{"files":[{"fileName":"one.png"},{"fileName":"missing.png"},{"fileName":"two.png"}],"concurrency":2}`)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			var result BatchFileResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 3 || len(result.Files) != 3 {
				t.Fatalf("unexpected batch: %+v", result)
			}
			for index, name := range []string{"one.png", "missing.png", "two.png"} {
				file := result.Files[index]
				if file.FileName != name {
					t.Fatal("input order not retained")
				}
				if index == 1 {
					if file.Error != "file or share not found" {
						t.Fatalf("error %q", file.Error)
					}
					continue
				}
				data, err := base64.StdEncoding.DecodeString(file.Content)
				if err != nil || string(data) != name {
					t.Fatal("content mismatch")
				}
			}
		})
	}
}
func TestProductionSingleRouteAndMIME(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nimage")
	reader := readerFunc(func(ctx context.Context, share, file string) ([]byte, error) { return data, nil })
	resp := request(t, newApp(testConfig(), reader, context.Background()), "GET", "/api/v1/smb/images/photo.png", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || string(body) != string(data) {
		t.Fatal("incorrect binary response or MIME type")
	}
}
func TestReadFailuresDoNotStopApp(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"notfound", fmt.Errorf("private: %w", fs.ErrNotExist), 404},
		{"denied", fs.ErrPermission, 403}, {"offline", errors.New("private-server secret"), 502},
		{"timeout", context.DeadlineExceeded, 504}, {"canceled", context.Canceled, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newApp(testConfig(), readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, tc.err }), context.Background())
			resp := request(t, app, "GET", "/api/v1/smb/images/file", "")
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || strings.Contains(string(body), "private") || strings.Contains(string(body), "secret") {
				t.Fatalf("status %d body %s", resp.StatusCode, body)
			}
			if resp := request(t, app, "GET", "/api/v1/hello", ""); resp.StatusCode != 200 {
				t.Fatal("app did not survive failure")
			}
		})
	}
}
func TestInvalidBatchDoesNotCallSMB(t *testing.T) {
	app := newApp(testConfig(), readerFunc(func(context.Context, string, string) ([]byte, error) { t.Error("unexpected SMB call"); return nil, nil }), context.Background())
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/smb/images/batch", "{"},
		{"POST", "/api/v1/smb/images/batch", `{"files":[]}`},
		{"POST", "/api/v1/smb/images/batch", `{"files":[{"fileName":" "}]}`},
		{"GET", "/api/v1/smb/images/batch", ""},
	} {
		if resp := request(t, app, tc.method, tc.path, tc.body); resp.StatusCode != 400 {
			t.Fatalf("got %d", resp.StatusCode)
		}
	}
}
func TestRequestDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.RequestTimeout = 20 * time.Millisecond
	var calls atomic.Int32
	reader := readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	app := newApp(cfg, reader, context.Background())
	if resp := request(t, app, "GET", "/api/v1/smb/images/file", ""); resp.StatusCode != 504 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	resp := request(t, app, "POST", "/api/v1/smb/images/batch", `{"files":[{"fileName":"a"},{"fileName":"b"},{"fileName":"c"}],"concurrency":1}`)
	var result BatchFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 3 {
		t.Fatal("missing timeout results")
	}
	for _, file := range result.Files {
		if file.Error != "SMB request timed out" {
			t.Fatalf("unexpected error %q", file.Error)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("expired queued work must not dial SMB; got %d calls", calls.Load())
	}
}
func TestWorkerPanicIsContained(t *testing.T) {
	app := newApp(testConfig(), readerFunc(func(context.Context, string, string) ([]byte, error) { panic("sensitive") }), context.Background())
	resp := request(t, app, "POST", "/api/v1/smb/images/batch", `{"files":[{"fileName":"a"}]}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "SMB request failed") || strings.Contains(string(body), "sensitive") {
		t.Fatalf("unsafe panic response %s", body)
	}
	resp = request(t, app, "GET", "/api/v1/smb/images/a", "")
	if resp.StatusCode != 500 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}
