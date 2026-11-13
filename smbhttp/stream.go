package smbhttp

import (
	"errors"
	"io"
	"net/http"
	"strconv"
)

// Fixed-size sniff/copy buffers avoid retaining whole files. No GET content
// length is promised from mutable opening metadata: completion is marked by the
// HTTP framing. Late errors abort the response so clients see an incomplete body.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, opener FileOpener, share, name string) {
	committed := false
	defer func() {
		if p := recover(); p != nil {
			if committed {
				panic(http.ErrAbortHandler)
			}
			panic(p)
		}
	}()
	file, err := opener.Open(r.Context(), share, name)
	if err != nil {
		status, message := publicReadError(err)
		writeError(w, r, status, message)
		return
	}
	defer file.Close()
	if r.Method == http.MethodHead {
		info, err := file.Stat()
		if err != nil {
			status, message := publicReadError(err)
			writeError(w, r, status, message)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		return
	}
	var prefix [512]byte
	n, err := sniff(file, prefix[:])
	if err != nil && err != io.EOF {
		status, message := publicReadError(err)
		writeError(w, r, status, message)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(prefix[:n]))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	committed = true
	w.WriteHeader(http.StatusOK)
	if n > 0 {
		if _, err := w.Write(prefix[:n]); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		panic(http.ErrAbortHandler)
	}
	if err == io.EOF {
		return
	}
	var buffer [32 << 10]byte
	for {
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		n, readErr := file.Read(buffer[:])
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			if writeErr != nil || written != n {
				panic(http.ErrAbortHandler)
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func sniff(reader io.Reader, buffer []byte) (int, error) {
	total, empty := 0, 0
	for total < len(buffer) {
		n, err := reader.Read(buffer[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			empty++
			if empty >= 100 {
				return total, io.ErrNoProgress
			}
		} else {
			empty = 0
		}
	}
	return total, nil
}
