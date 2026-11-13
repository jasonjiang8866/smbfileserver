package client

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func localStream(t *testing.T, contents string, limit int64) (*stream, *atomic.Int32) {
	t.Helper()
	file, err := (fstest.MapFS{"f": &fstest.MapFile{Data: []byte(contents)}}).Open("f")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := file.Stat()
	ctx, cancel := context.WithCancel(context.Background())
	count := &atomic.Int32{}
	s := newStream(ctx, cancel, file, info, limit, func() {}, func() { count.Add(1) })
	t.Cleanup(func() { s.Close() })
	return s, count
}
func TestStreamEOFAndIdempotentClose(t *testing.T) {
	s, count := localStream(t, "12345678", 8)
	data, err := io.ReadAll(s)
	if err != nil || string(data) != "12345678" || count.Load() != 1 {
		t.Fatalf("EOF cleanup: %q %v count=%d", data, err, count.Load())
	}
	s.Close()
	s.Close()
	if count.Load() != 1 {
		t.Fatal("cleanup ran more than once")
	}
	if _, err := s.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("repeated EOF: %v", err)
	}
}
func TestStreamGrowthLimit(t *testing.T) {
	s, count := localStream(t, "123456789", 8)
	data, err := io.ReadAll(s)
	if !errors.Is(err, ErrTooLarge) || string(data) != "12345678" || count.Load() != 1 {
		t.Fatalf("growth: %q %v", data, err)
	}
}
func TestStreamEarlyClose(t *testing.T) {
	s, count := localStream(t, "remaining data", 100)
	if _, err := s.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if count.Load() != 1 {
		t.Fatal("early close leaked")
	}
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
}

type blockedFile struct {
	fs.File
	started, abort chan struct{}
	once           sync.Once
}

func (f *blockedFile) Read([]byte) (int, error) {
	f.once.Do(func() { close(f.started) })
	<-f.abort
	return 0, io.ErrUnexpectedEOF
}
func TestStreamCloseInterruptsRead(t *testing.T) {
	raw, _ := (fstest.MapFS{"f": &fstest.MapFile{Data: []byte("x")}}).Open("f")
	info, _ := raw.Stat()
	f := &blockedFile{File: raw, started: make(chan struct{}), abort: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	var count atomic.Int32
	s := newStream(ctx, cancel, f, info, 10, func() { once.Do(func() { close(f.abort) }) }, func() { count.Add(1) })
	defer s.Close()
	read := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 1)); read <- err }()
	<-f.started
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind Read")
	}
	if err := <-read; !errors.Is(err, fs.ErrClosed) {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatal("admission cleanup repeated")
	}
}
