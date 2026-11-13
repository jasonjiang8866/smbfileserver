package smbhttp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

type openReader struct {
	open     func(context.Context) (fs.File, error)
	readFile func(context.Context) ([]byte, error)
	buffered atomic.Int32
}

func (o *openReader) Open(ctx context.Context, _, _ string) (fs.File, error) { return o.open(ctx) }
func (o *openReader) ReadFile(ctx context.Context, _, _ string) ([]byte, error) {
	o.buffered.Add(1)
	if o.readFile != nil {
		return o.readFile(ctx)
	}
	return []byte("buffered"), nil
}

// Use an actual HTTP/1.0 connection: ResponseRecorder cannot detect whether
// close-delimited transport framing hides a truncated response.
func http10Response(t *testing.T, server *httptest.Server, method string) *http.Response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, method+" /s/f HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestHTTP10GETBuffersBeforeHeaders(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "upstream_failure"
		}
		t.Run(name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), 1024)
			var closed atomic.Int32
			source := &openReader{open: func(context.Context) (fs.File, error) {
				reader := bytes.NewReader(payload)
				return &fakeStream{File: fixture(), read: func(p []byte) (int, error) {
					if fail && reader.Len() <= 512 {
						return 0, errors.New("secret upstream failure")
					}
					if len(p) > 512 {
						p = p[:512]
					}
					return reader.Read(p)
				}, close: func() error { closed.Add(1); return nil }}, nil
			}}
			// Like client.ReadFile, consume the same source and discard data on
			// failure. The handler must not call the streaming path for HTTP/1.0.
			source.readFile = func(ctx context.Context) ([]byte, error) {
				file, err := source.open(ctx)
				if err != nil {
					return nil, err
				}
				defer file.Close()
				data, err := io.ReadAll(file)
				if err != nil {
					return nil, err
				}
				return data, nil
			}
			server := httptest.NewServer(handler(t, source, smbhttp.Options{}))
			t.Cleanup(server.Close)
			response := http10Response(t, server, http.MethodGet)
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("incomplete response: %v", err)
			}
			wantStatus, wantBody := http.StatusOK, payload
			if fail {
				wantStatus, wantBody = http.StatusBadGateway, []byte(`{"error":"SMB request failed"}`)
				if response.Header.Get("Content-Type") != "application/json" {
					t.Fatal("failure must be JSON")
				}
			}
			if response.StatusCode != wantStatus || !bytes.Equal(data, wantBody) || response.ContentLength != int64(len(wantBody)) {
				t.Fatalf("status=%d length=%d body=%q", response.StatusCode, response.ContentLength, data)
			}
			if source.buffered.Load() != 1 || closed.Load() != 1 {
				t.Fatalf("buffered calls=%d closes=%d", source.buffered.Load(), closed.Load())
			}
		})
	}
}

func TestHTTP10HEADRemainsMetadataOnly(t *testing.T) {
	var reads, closed atomic.Int32
	source := &openReader{open: func(context.Context) (fs.File, error) {
		return &fakeStream{File: fixture(), read: func([]byte) (int, error) {
			reads.Add(1)
			return 0, io.EOF
		}, close: func() error { closed.Add(1); return nil }}, nil
	}}
	server := httptest.NewServer(handler(t, source, smbhttp.Options{}))
	t.Cleanup(server.Close)
	response := http10Response(t, server, http.MethodHead)
	data, err := io.ReadAll(response.Body)
	if err != nil || len(data) != 0 || response.StatusCode != 200 || response.ContentLength != 8 || response.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("HEAD status=%d length=%d body=%q err=%v", response.StatusCode, response.ContentLength, data, err)
	}
	if reads.Load() != 0 || source.buffered.Load() != 0 || closed.Load() != 1 {
		t.Fatal("HEAD read file contents or leaked stream")
	}
}

type fakeStream struct {
	fs.File
	read  func([]byte) (int, error)
	close func() error
}

func (f *fakeStream) Read(p []byte) (int, error) { return f.read(p) }
func (f *fakeStream) Close() error               { return f.close() }
func fixture() fs.File {
	f, _ := (fstest.MapFS{"f": &fstest.MapFile{Data: []byte("metadata")}}).Open("f")
	return f
}

func TestStreamingFirstBytesAndDisconnect(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	var reads atomic.Int32
	source := &openReader{open: func(ctx context.Context) (fs.File, error) {
		return &fakeStream{File: fixture(), read: func(p []byte) (int, error) {
			if reads.Add(1) == 1 {
				return copy(p, bytes.Repeat([]byte("x"), 512)), nil
			}
			<-ctx.Done()
			return 0, ctx.Err()
		}, close: func() error { once.Do(func() { close(closed) }); return nil }}, nil
	}}
	server := httptest.NewServer(handler(t, source, smbhttp.Options{RequestTimeout: 5 * time.Second}))
	defer server.Close()
	client := http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL + "/s/f")
	if err != nil {
		t.Fatalf("first bytes blocked on complete read: %v", err)
	}
	prefix := make([]byte, 128)
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not close stream")
	}
	if source.buffered.Load() != 0 {
		t.Fatal("streaming capability not selected")
	}
}
func TestStreamingFailureAbortsBody(t *testing.T) {
	for _, panicAfter := range []bool{false, true} {
		var reads int
		var closed atomic.Int32
		source := &openReader{open: func(context.Context) (fs.File, error) {
			return &fakeStream{File: fixture(), read: func(p []byte) (int, error) {
				reads++
				if reads == 1 {
					return copy(p, bytes.Repeat([]byte("x"), 512)), nil
				}
				if panicAfter {
					panic("secret")
				}
				return 0, errors.New("secret")
			}, close: func() error { closed.Add(1); return nil }}, nil
		}}
		server := httptest.NewServer(handler(t, source, smbhttp.Options{}))
		response, err := (&http.Client{Timeout: time.Second}).Get(server.URL + "/s/f")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		server.Close()
		if err == nil || response.StatusCode != 200 || bytes.Contains(data, []byte("secret")) || bytes.Contains(data, []byte("error")) {
			t.Fatalf("late failure appeared successful: %q %v", data, err)
		}
		if closed.Load() != 1 {
			t.Fatal("failure leaked stream")
		}
	}
}
func TestStreamingHEADAndFallback(t *testing.T) {
	var reads, closed atomic.Int32
	source := &openReader{open: func(context.Context) (fs.File, error) {
		return &fakeStream{File: fixture(), read: func([]byte) (int, error) { reads.Add(1); return 0, io.EOF }, close: func() error { closed.Add(1); return nil }}, nil
	}}
	h := handler(t, source, smbhttp.Options{})
	w := call(h, "HEAD", "/s/f", "")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "8" || reads.Load() != 0 || closed.Load() != 1 {
		t.Fatal("HEAD read body or leaked")
	}
	h = handler(t, source, smbhttp.Options{DisableStreaming: true})
	if w := call(h, "GET", "/s/f", ""); w.Body.String() != "buffered" || source.buffered.Load() != 1 {
		t.Fatal("explicit fallback not used")
	}
}
func TestBatchLimits(t *testing.T) {
	var calls atomic.Int32
	reader := readerFunc(func(context.Context, string, string) ([]byte, error) { calls.Add(1); return []byte("1234"), nil })
	h := handler(t, reader, smbhttp.Options{MaxBatchFiles: 2, MaxBatchBytes: 6})
	w := call(h, "POST", "/s/batch", `{"files":[{"fileName":"a"},{"fileName":"b"},{"fileName":"c"}]}`)
	if w.Code != 413 || calls.Load() != 0 {
		t.Fatal("file-count limit did not reject early")
	}
	w = call(h, "POST", "/s/batch", `{"files":[{"fileName":"a"},{"fileName":"b"}],"concurrency":1}`)
	var result smbhttp.BatchFileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Files[0].Content != "MTIzNA==" || result.Files[1].Error != "batch byte limit exceeded" {
		t.Fatalf("batch budget: %+v", result)
	}
	total := 0
	for _, f := range result.Files {
		data, _ := base64.StdEncoding.DecodeString(f.Content)
		total += len(data)
	}
	if total > 6 {
		t.Fatal("aggregate contents exceeded budget")
	}
}

// Generated bytes and a discard writer measure handler allocations rather than
// allocating a whole fixture/HTTP response in the benchmark itself.
type generatedFile struct {
	fs.File
	remaining int64
	maxRead   int
}

func (f *generatedFile) Read(p []byte) (int, error) {
	if len(p) > f.maxRead {
		f.maxRead = len(p)
	}
	if f.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > f.remaining {
		n = f.remaining
	}
	clear(p[:n])
	f.remaining -= n
	return int(n), nil
}

type discardWriter struct{ header http.Header }

func (w *discardWriter) Header() http.Header       { return w.header }
func (*discardWriter) WriteHeader(int)             {}
func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*discardWriter) Flush()                      {}
func BenchmarkStreaming(b *testing.B) {
	for _, size := range []int64{8 << 20, 64 << 20} {
		b.Run(strconv.FormatInt(size>>20, 10)+"MiB", func(b *testing.B) {
			source := &openReader{open: func(context.Context) (fs.File, error) { return &generatedFile{File: fixture(), remaining: size}, nil }}
			h, _ := smbhttp.New(source, smbhttp.Options{})
			r := httptest.NewRequest("GET", "/s/f", nil)
			b.ReportAllocs()
			b.SetBytes(size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.ServeHTTP(&discardWriter{header: make(http.Header)}, r)
			}
		})
	}
}
func TestStreamingUsesFixedReadBuffer(t *testing.T) {
	file := &generatedFile{File: fixture(), remaining: 8 << 20}
	source := &openReader{open: func(context.Context) (fs.File, error) { return file, nil }}
	h := handler(t, source, smbhttp.Options{})
	h.ServeHTTP(&discardWriter{header: make(http.Header)}, httptest.NewRequest("GET", "/s/f", nil))
	if file.maxRead > 32<<10 || source.buffered.Load() != 0 || file.remaining != 0 {
		t.Fatalf("unexpected buffering: maxRead=%d", file.maxRead)
	}
}

func TestStreamingErrorBeforeHeaders(t *testing.T) {
	var closed atomic.Int32
	source := &openReader{open: func(context.Context) (fs.File, error) {
		return &fakeStream{File: fixture(), read: func(p []byte) (int, error) { return copy(p, "short"), io.ErrUnexpectedEOF }, close: func() error { closed.Add(1); return nil }}, nil
	}}
	w := call(handler(t, source, smbhttp.Options{}), "GET", "/s/f", "")
	if w.Code != 502 || bytes.Contains(w.Body.Bytes(), []byte("short")) || closed.Load() != 1 {
		t.Fatal("pre-header failure was sent as a successful short file")
	}
}
func TestBatchStreamingSourceStopsAtBudget(t *testing.T) {
	var closed atomic.Int32
	source := &openReader{open: func(context.Context) (fs.File, error) {
		f := fixture()
		return &fakeStream{File: f, read: f.Read, close: func() error { closed.Add(1); return f.Close() }}, nil
	}}
	w := call(handler(t, source, smbhttp.Options{MaxBatchBytes: 3}), "POST", "/s/batch", `{"files":[{"fileName":"f"}]}`)
	var result smbhttp.BatchFileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Content != "" || result.Files[0].Error != "batch byte limit exceeded" || closed.Load() != 1 || source.buffered.Load() != 0 {
		t.Fatalf("unbounded batch read: %+v", result)
	}
}
