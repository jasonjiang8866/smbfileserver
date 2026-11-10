package smbhttp_test

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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

type readerFunc func(context.Context, string, string) ([]byte, error)

func (f readerFunc) ReadFile(ctx context.Context, share, file string) ([]byte, error) {
	return f(ctx, share, file)
}
func handler(t *testing.T, reader smbhttp.FileReader, options smbhttp.Options) http.Handler {
	t.Helper()
	h, err := smbhttp.New(reader, options)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func call(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMountAndBinaryResponse(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nimage")
	h := handler(t, readerFunc(func(_ context.Context, s, f string) ([]byte, error) {
		if s != "images" || f != "晶圆 image.png" {
			t.Errorf("wrong reader args %q %q", s, f)
		}
		return data, nil
	}), smbhttp.Options{})
	mux := http.NewServeMux()
	mux.Handle("/files/", http.StripPrefix("/files", h))
	for _, method := range []string{"GET", "HEAD"} {
		w := call(mux, method, "/files/images/晶圆%20image.png", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %v", method, w)
		}
		if method == "GET" && !bytes.Equal(w.Body.Bytes(), data) {
			t.Fatal("binary data changed")
		}
		if method == "HEAD" && (w.Body.Len() != 0 || w.Header().Get("Content-Length") != fmt.Sprint(len(data))) {
			t.Fatal("invalid HEAD")
		}
	}
}
func TestBatchOrderingErrorsAndConcurrency(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			var active, peak, calls atomic.Int32
			h := handler(t, readerFunc(func(_ context.Context, s, f string) ([]byte, error) {
				if s != "share" || f == "batch" {
					t.Errorf("wrong route %s/%s", s, f)
				}
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if f == "first" {
					time.Sleep(10 * time.Millisecond)
				}
				if f == "missing" {
					return nil, fmt.Errorf("secret: %w", fs.ErrNotExist)
				}
				return []byte(f), nil
			}), smbhttp.Options{})
			w := call(h, method, "/share/batch?files=first,missing,first,nested/工艺.csv&concurrency=2", `{"files":[{"fileName":"first"},{"fileName":"missing"},{"fileName":"first"},{"fileName":"nested/工艺.csv"}],"concurrency":2}`)
			var got smbhttp.BatchFileResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Files) != 4 {
				t.Fatalf("%v", w)
			}
			for i, name := range []string{"first", "missing", "first", "nested/工艺.csv"} {
				f := got.Files[i]
				if f.FileName != name {
					t.Fatal("lost input order")
				}
				if i == 1 {
					if f.Error != "file or share not found" {
						t.Fatal(f)
					}
					continue
				}
				b, e := base64.StdEncoding.DecodeString(f.Content)
				if e != nil || string(b) != name || f.Error != "" {
					t.Fatal(f)
				}
			}
			if calls.Load() != 4 || peak.Load() > 2 || peak.Load() < 2 {
				t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
			}
		})
	}
}
func TestRejectsInvalidInputBeforeReader(t *testing.T) {
	h := handler(t, readerFunc(func(context.Context, string, string) ([]byte, error) {
		t.Error("unexpected backend call")
		return nil, nil
	}), smbhttp.Options{MaxBodyBytes: 100})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/", "", 404}, {"GET", "/share", "", 404}, {"GET", "/share/a/b", "", 404},
		{"DELETE", "/share/file", "", 405}, {"POST", "/share/file", `{}`, 405},
		{"GET", "/share/batch", "", 400}, {"POST", "/share/batch", `{`, 400},
		{"POST", "/share/batch", `{"files":[]}`, 400}, {"POST", "/share/batch", `{"files":[{"fileName":" "}]}`, 400},
		{"POST", "/share/batch", `{"files":[{"fileName":"a"}]} {}`, 400},
		{"POST", "/share/batch", `{"files":[{"fileName":"` + strings.Repeat("a", 101) + `"}]}`, 413},
	} {
		w := call(h, tc.method, tc.path, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
		if tc.status == 405 && w.Header().Get("Allow") == "" {
			t.Error("missing Allow")
		}
	}
	r := httptest.NewRequest("POST", "/share/batch", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w)
	}
}
func TestErrorRedactionAndPanics(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("secret: %w", fs.ErrNotExist), 404}, {fs.ErrPermission, 403},
		{context.DeadlineExceeded, 504}, {context.Canceled, 503}, {&net.DNSError{IsTimeout: true}, 504}, {errors.New("secret"), 502},
	} {
		h := handler(t, readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, tc.err }), smbhttp.Options{})
		w := call(h, "GET", "/share/file", "")
		if w.Code != tc.status || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w)
		}
	}
	h := handler(t, readerFunc(func(context.Context, string, string) ([]byte, error) { panic("secret") }), smbhttp.Options{})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/s/f", "", 500}, {"POST", "/s/batch", `{"files":[{"fileName":"a"}]}`, 200}} {
		w := call(h, tc.method, tc.path, tc.body)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "secret") {
			t.Fatal(w)
		}
	}
}
func TestContextValuesDeadlineAndQueuedCancellation(t *testing.T) {
	type key struct{}
	var calls atomic.Int32
	h := handler(t, readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		calls.Add(1)
		if ctx.Value(key{}) != "identity" {
			t.Error("context value lost")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), smbhttp.Options{RequestTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "identity"), 30*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/s/batch", strings.NewReader(`{"files":[{"fileName":"a"},{"fileName":"b"}],"concurrency":1}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var result smbhttp.BatchFileResponse
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Files) != 2 || calls.Load() != 1 {
		t.Fatalf("queued work started: %s", w.Body)
	}
	for _, f := range result.Files {
		if f.Error != "SMB request timed out" {
			t.Fatal(f)
		}
	}
	// A pre-canceled request must not reach the backend.
	canceled, stop := context.WithCancel(context.Background())
	stop()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/s/f", nil).WithContext(canceled))
	if w.Code != 503 || calls.Load() != 1 {
		t.Fatal("pre-canceled read reached backend")
	}
}
func TestHandlerTimeout(t *testing.T) {
	h := handler(t, readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }), smbhttp.Options{RequestTimeout: 10 * time.Millisecond})
	if w := call(h, "GET", "/s/f", ""); w.Code != 504 {
		t.Fatal(w)
	}
}
func TestClientDisconnectCancelsReader(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	h := handler(t, readerFunc(func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	}), smbhttp.Options{RequestTimeout: 5 * time.Second})
	server := httptest.NewServer(h)
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "GET /share/file HTTP/1.1\r\nHost: localhost\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	_ = conn.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel reader")
	}
}
func TestConstructorValidation(t *testing.T) {
	reader := readerFunc(func(context.Context, string, string) ([]byte, error) { return nil, nil })
	if _, err := smbhttp.New(nil, smbhttp.Options{}); err == nil {
		t.Fatal("nil reader accepted")
	}
	for _, o := range []smbhttp.Options{{RequestTimeout: -1}, {MaxBodyBytes: -1}} {
		if _, err := smbhttp.New(reader, o); err == nil {
			t.Fatal("negative limit accepted")
		}
	}
}

// ExampleNew shows embedding without importing Fiber or the SMB implementation.
func ExampleNew() {
	reader := readerFunc(func(_ context.Context, share, file string) ([]byte, error) {
		if share == "reports" && file == "hello.txt" {
			return []byte("hello"), nil
		}
		return nil, fs.ErrNotExist
	})
	h, err := smbhttp.New(reader, smbhttp.Options{})
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/files/", http.StripPrefix("/files", h))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/files/reports/hello.txt", nil))
	fmt.Println(response.Code, response.Body.String())
	// Output: 200 hello
}

func TestPathDecodedOnce(t *testing.T) {
	var calls int
	h := handler(t, readerFunc(func(_ context.Context, share, file string) ([]byte, error) {
		calls++
		if share != "share" || file != "literal%2Fname.txt" {
			t.Fatalf("unexpected decode: %q %q", share, file)
		}
		return nil, nil
	}), smbhttp.Options{})
	if w := call(h, "GET", "/share/literal%252Fname.txt", ""); w.Code != 200 {
		t.Fatal(w)
	}
	if w := call(h, "GET", "/share/nested%2Ffile.txt", ""); w.Code != 404 {
		t.Fatal(w)
	}
	if calls != 1 {
		t.Fatal("encoded slash reached reader")
	}
}
