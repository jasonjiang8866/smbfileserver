// Package smbhttp exposes a read-only file API as a standard http.Handler.
// It does not load configuration, open listeners or depend on an SMB backend.
// Applications supply a concurrent, cancellation-aware FileReader and wrap the
// handler with their authentication and authorization middleware.
package smbhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileReader reads one file from a share. Implementations must support concurrent
// calls and honor ctx cancellation. The caller owns the reader's lifecycle.
// Return errors wrapping fs.ErrNotExist, fs.ErrPermission or context errors to
// preserve HTTP error classification. Other errors become a sanitized 502.
type FileReader interface {
	ReadFile(ctx context.Context, share, file string) ([]byte, error)
}

// Options configures one handler. Zero values select the documented defaults.
type Options struct {
	// RequestTimeout bounds a whole request, including queued batch work (default 30s).
	// An earlier deadline on the incoming request always takes precedence.
	RequestTimeout time.Duration
	// MaxBodyBytes limits POST JSON input (default 4 MiB). This does not bound
	// downloaded file sizes or response memory usage.
	MaxBodyBytes int64
}

// Handler serves /{share}/{file} (GET/HEAD) and /{share}/batch (GET/HEAD/POST).
// Mount at any prefix with http.StripPrefix. A file occupies one URL segment;
// nested filenames can be supplied in a batch. The literal "batch" is reserved.
// Handler is safe for concurrent use when its FileReader is.
type Handler struct {
	reader  FileReader
	options Options
}

var _ http.Handler = (*Handler)(nil)

// New constructs an independent handler without changing process state.
// Negative limits and a nil reader are rejected. No network I/O occurs here.
func New(reader FileReader, options Options) (*Handler, error) {
	if reader == nil {
		return nil, errors.New("smbhttp: reader is required")
	}
	if options.RequestTimeout < 0 || options.MaxBodyBytes < 0 {
		return nil, errors.New("smbhttp: limits must not be negative")
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = 4 << 20
	}
	return &Handler{reader: reader, options: options}, nil
}

// ServeHTTP handles paths relative to the application's chosen mount point.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			writeError(w, r, 500, "internal server error")
		}
	}()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, r, 404, "Not Found")
		return
	}
	batch := parts[1] == "batch"
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(batch && r.Method == http.MethodPost) {
		allowed := "GET, HEAD"
		if batch {
			allowed += ", POST"
		}
		w.Header().Set("Allow", allowed)
		writeError(w, r, 405, "Method Not Allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.options.RequestTimeout)
	defer cancel()
	if batch {
		h.batch(w, r.WithContext(ctx), parts[0])
		return
	}
	if err := ctx.Err(); err != nil {
		status, message := publicReadError(err)
		writeError(w, r, status, message)
		return
	}
	data, err := h.reader.ReadFile(ctx, parts[0], parts[1])
	if err != nil {
		status, message := publicReadError(err)
		writeError(w, r, status, message)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) batch(w http.ResponseWriter, r *http.Request, share string) {
	var req BatchFileRequest
	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, r, 415, "Content-Type must be application/json")
			return
		}
		body := http.MaxBytesReader(w, r.Body, h.options.MaxBodyBytes)
		defer body.Close()
		decoder := json.NewDecoder(body)
		err = decoder.Decode(&req)
		if err == nil {
			var extra any
			if next := decoder.Decode(&extra); next != io.EOF {
				err = next
				if err == nil {
					err = errors.New("multiple JSON values")
				}
			}
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, r, 413, "request body too large")
			} else {
				writeError(w, r, 400, "Bad Request")
			}
			return
		}
	} else {
		req.Concurrency, _ = strconv.Atoi(r.URL.Query().Get("concurrency"))
		for _, file := range strings.Split(r.URL.Query().Get("files"), ",") {
			if name := strings.TrimSpace(file); name != "" {
				req.Files = append(req.Files, FileRequest{FileName: name})
			}
		}
	}
	if len(req.Files) == 0 {
		writeError(w, r, 400, "no files specified")
		return
	}
	for _, file := range req.Files {
		if strings.TrimSpace(file.FileName) == "" {
			writeError(w, r, 400, "fileName must not be empty")
			return
		}
	}
	writeJSON(w, r, 200, readBatch(r.Context(), h.reader, share, req))
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, r, status, struct {
		Error string `json:"error"`
	}{message})
}
func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = 500
		data = []byte(`{"error":"internal server error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// FileRequest identifies a file within the requested share.
type FileRequest struct {
	FileName string `json:"fileName"`
}

// BatchFileRequest is the POST JSON payload.
type BatchFileRequest struct {
	Files       []FileRequest `json:"files"`
	Concurrency int           `json:"concurrency,omitempty"`
}

// FileResponse contains Base64 content or a sanitized per-file error.
type FileResponse struct {
	FileName string `json:"fileName"`
	Content  string `json:"content"`
	Error    string `json:"error,omitempty"`
}

// BatchFileResponse preserves input order and duplicate entries.
type BatchFileResponse struct {
	Files []FileResponse `json:"files"`
}

func readBatch(ctx context.Context, reader FileReader, share string, req BatchFileRequest) BatchFileResponse {
	workers := req.Concurrency
	if workers <= 0 {
		workers = 3
	}
	if workers > 10 {
		workers = 10
	}
	if workers > len(req.Files) {
		workers = len(req.Files)
	}
	response := BatchFileResponse{Files: make([]FileResponse, len(req.Files))}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				file := req.Files[index]
				result := FileResponse{FileName: file.FileName}
				data, err := readBatchFile(ctx, reader, share, file.FileName)
				if err != nil {
					_, result.Error = publicReadError(err)
				} else {
					result.Content = base64.StdEncoding.EncodeToString(data)
				}
				response.Files[index] = result
			}
		}()
	}
	// Each worker writes a distinct index; output order follows input order.
	for index := range req.Files {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return response
}

func readBatchFile(ctx context.Context, reader FileReader, share, file string) (data []byte, err error) {
	// HTTP recovery middleware cannot catch a panic in a separate worker goroutine.
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("file reader failed")
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return reader.ReadFile(ctx, share, file)
}

func publicReadError(err error) (int, string) {
	var networkError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "SMB request timed out"
	case errors.Is(err, context.Canceled):
		return 503, "request canceled"
	case errors.As(err, &networkError) && networkError.Timeout():
		return 504, "SMB request timed out"
	case errors.Is(err, fs.ErrNotExist):
		return 404, "file or share not found"
	case errors.Is(err, fs.ErrPermission):
		return 403, "file access denied"
	default:
		return 502, "SMB request failed"
	}
}
