package client

import (
	"context"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"
)

type streamEnd struct{ err error }

type stream struct {
	ctx            context.Context
	cancel         context.CancelFunc
	file           fs.File
	info           fileInfo
	remaining      int64
	abort, cleanup func()
	readMu         sync.Mutex
	once           sync.Once
	terminal       atomic.Pointer[streamEnd]
}

func newStream(ctx context.Context, cancel context.CancelFunc, file fs.File, info fs.FileInfo, limit int64, abort, cleanup func()) *stream {
	return &stream{ctx: ctx, cancel: cancel, file: file, info: fileInfo{info.Name(), info.Size(), info.Mode(), info.ModTime()}, remaining: limit, abort: abort, cleanup: cleanup}
}

// Reads are serialized. Close first interrupts the transport, so it can unblock
// a stalled Read before waiting for the read mutex and releasing SMB handles.
func (s *stream) Read(p []byte) (int, error) {
	n, err := s.read(p)
	if err != nil {
		_ = s.Close()
	}
	return n, err
}

func (s *stream) read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if end := s.terminal.Load(); end != nil {
		return 0, end.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	var err error
	if s.ctx.Err() != nil {
		err = opError(s.ctx, "read", s.ctx.Err())
	} else if s.remaining == 0 {
		var probe [1]byte
		n, err = s.file.Read(probe[:])
		if n > 0 {
			err = &OpError{Op: "read", Err: ErrTooLarge}
		}
		n = 0
	} else {
		if int64(len(p)) > s.remaining {
			p = p[:s.remaining]
		}
		n, err = s.file.Read(p)
		s.remaining -= int64(n)
	}
	if err != nil && err != io.EOF {
		err = opError(s.ctx, "read", err)
	}
	if err != nil {
		s.terminal.CompareAndSwap(nil, &streamEnd{err})
	}
	if end := s.terminal.Load(); end != nil {
		err = end.err
	}
	return n, err
}

// Stat returns the opening metadata snapshot, including after the stream closes.
func (s *stream) Stat() (fs.FileInfo, error) { return s.info, nil }

func (s *stream) Close() error {
	reason := error(fs.ErrClosed)
	if s.ctx.Err() != nil {
		reason = opError(s.ctx, "read", s.ctx.Err())
	}
	s.terminal.CompareAndSwap(nil, &streamEnd{reason})
	s.cancel()
	s.abort() // Idempotent transport close, also interrupts a concurrent Read.
	s.once.Do(func() {
		s.readMu.Lock()
		defer s.readMu.Unlock()
		_ = s.file.Close()
		s.cleanup()
	})
	return nil
}

// Expose standard metadata only, not the backend's Sys payload.
type fileInfo struct {
	name     string
	size     int64
	mode     fs.FileMode
	modified time.Time
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.modified }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }
