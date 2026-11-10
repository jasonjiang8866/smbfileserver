package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"testing"
	"testing/fstest"

	"github.com/cloudsoda/go-smb2"
)

type advertisedFile struct {
	fs.File
	size  int64
	reads int
}
type advertisedInfo struct {
	fs.FileInfo
	size int64
}

func (i advertisedInfo) Size() int64 { return i.size }
func (f *advertisedFile) Stat() (fs.FileInfo, error) {
	i, e := f.File.Stat()
	return advertisedInfo{i, f.size}, e
}
func (f *advertisedFile) Read(p []byte) (int, error) { f.reads++; return f.File.Read(p) }
func TestReadLimitChecksMetadataAndActualBytes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		actual     int
		advertised int64
		want       error
		noRead     bool
	}{
		{"empty", 0, 0, nil, false}, {"exact boundary", 8, 8, nil, false}, {"below limit", 7, 7, nil, false},
		{"known oversize", 9, 9, ErrTooLarge, true}, {"grown since stat", 9, 4, ErrTooLarge, false},
		{"reported empty but oversized", 9, 0, ErrTooLarge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := bytes.Repeat([]byte("x"), tc.actual)
			raw, e := (fstest.MapFS{"f": &fstest.MapFile{Data: content}}).Open("f")
			if e != nil {
				t.Fatal(e)
			}
			defer raw.Close()
			file := &advertisedFile{File: raw, size: tc.advertised}
			data, err := readBounded(file, 8)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if err != nil && data != nil {
				t.Fatal("partial file returned on failure")
			}
			if err == nil && !bytes.Equal(data, content) {
				t.Fatal("content changed")
			}
			if tc.noRead && file.reads != 0 {
				t.Fatal("oversized metadata still read contents")
			}
		})
	}
}
func TestErrorNormalization(t *testing.T) {
	for _, tc := range []struct{ err, want error }{
		{fmt.Errorf("secret: %w", fs.ErrNotExist), fs.ErrNotExist}, {fs.ErrPermission, fs.ErrPermission},
		{fs.ErrInvalid, fs.ErrInvalid}, {ErrTooLarge, ErrTooLarge}, {context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded}, {&smb2.TransportError{Err: &net.DNSError{IsTimeout: true}}, context.DeadlineExceeded},
		{&smb2.TransportError{Err: io.EOF}, ErrUnavailable}, {errors.New("secret backend"), ErrUnavailable},
	} {
		err := opError(context.Background(), "read", tc.err)
		if !errors.Is(err, tc.want) {
			t.Fatalf("%v: %v", tc.err, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := opError(ctx, "read", io.EOF); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var limit interface{ SizeLimitExceeded() bool }
	if err := opError(context.Background(), "read", ErrTooLarge); !errors.As(err, &limit) || !limit.SizeLimitExceeded() {
		t.Fatal("size marker lost")
	}
}

type failingFile struct{ statReader }

func (f failingFile) Read(p []byte) (int, error) { copy(p, "xx"); return 2, io.ErrUnexpectedEOF }
func TestReadFailureReturnsNoPartialContents(t *testing.T) {
	raw, err := (fstest.MapFS{"f": &fstest.MapFile{Data: []byte("xxxx")}}).Open("f")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	data, err := readBounded(failingFile{raw}, 8)
	if data != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial failure returned %q, %v", data, err)
	}
}
