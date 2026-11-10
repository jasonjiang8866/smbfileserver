// Package client provides bounded, read-only SMB file reads backed by CloudSoda.
// Each operation owns its connection, session, share and file handle. A Client
// may be reused concurrently, but must not be copied after construction.
// It reads no environment variables and starts no persistent goroutines.
package client

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cloudsoda/go-smb2"
)

const (
	// DefaultTimeout covers admission, connection, authentication, read and cleanup.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxConcurrentReads bounds active operations on one Client instance.
	DefaultMaxConcurrentReads = 8
	// DefaultMaxFileBytes bounds the contents of one successfully returned file.
	DefaultMaxFileBytes int64 = 32 << 20
)

// Config is copied by New. Zero port/limits select defaults. Credentials are
// retained in memory for subsequent calls; create another Client to rotate them.
// Do not log Config or serialize credentials.
type Config struct {
	Host               string
	Port               int
	Username           string
	Password           string
	Domain             string
	Timeout            time.Duration
	MaxConcurrentReads int
	MaxFileBytes       int64
}

// Client implements smbhttp.FileReader structurally, without importing HTTP or
// Fiber. Construct it with New; the zero value is not usable. There is no Close
// method: ReadFile owns cleanup; callers of Open close each returned stream.
type Client struct {
	config Config
	slots  chan struct{}
}

// New validates configuration without contacting the SMB server. Host must be a
// bare hostname or IP; Port defaults to 445. Negative limits are invalid, and
// MaxFileBytes must be less than math.MaxInt64 to allow an overflow-detection byte.
func New(config Config) (*Client, error) {
	config.Host = strings.TrimSpace(config.Host)
	if config.Host == "" || strings.ContainsAny(config.Host, "/\\ \t\r\n[]\x00") || (strings.Contains(config.Host, ":") && net.ParseIP(config.Host) == nil) {
		return nil, errors.New("client: Host must be a bare hostname or IP address")
	}
	if config.Port == 0 {
		config.Port = 445
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("client: Port must be between 1 and 65535")
	}
	if strings.TrimSpace(config.Username) == "" {
		return nil, errors.New("client: Username is required")
	}
	if strings.TrimSpace(config.Password) == "" {
		return nil, errors.New("client: Password is required")
	}
	if config.Timeout < 0 {
		return nil, errors.New("client: Timeout must not be negative")
	}
	if config.MaxConcurrentReads < 0 {
		return nil, errors.New("client: MaxConcurrentReads must not be negative")
	}
	if config.MaxFileBytes < 0 || config.MaxFileBytes == math.MaxInt64 {
		return nil, errors.New("client: MaxFileBytes must be nonnegative and less than MaxInt64")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxConcurrentReads == 0 {
		config.MaxConcurrentReads = DefaultMaxConcurrentReads
	}
	if config.MaxFileBytes == 0 {
		config.MaxFileBytes = DefaultMaxFileBytes
	}
	return &Client{config: config, slots: make(chan struct{}, config.MaxConcurrentReads)}, nil
}

// ReadFile is the buffered convenience API built on Open. It returns the whole
// file or nil on failure. Use Open for incremental transfer without retaining it.
func (c *Client) ReadFile(parent context.Context, shareName, fileName string) ([]byte, error) {
	file, err := c.Open(parent, shareName, fileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := readBounded(file, c.config.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Open returns a read-only stream and cached metadata. Call Close even after
// partial reads. EOF, read failure, context cancellation and timeout also release
// resources automatically. The admission slot is held for the stream's lifetime.
// The timeout starts before admission and continues after Open returns.
// Each call uses a fresh session; no retries or pooling are performed.
// A bare share name is required; nested file paths are supported but not confined.
func (c *Client) Open(parent context.Context, shareName, fileName string) (fs.File, error) {
	ctx, cancel := context.WithTimeout(parent, c.config.Timeout)
	transferred := false
	var cleanups []func()
	var stop func() bool
	defer func() {
		if !transferred {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
			cancel()
		}
		if stop != nil {
			stop()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, opError(ctx, "admit", err)
	}
	if strings.TrimSpace(shareName) == "" || strings.ContainsAny(shareName, "/\\\x00") || strings.TrimSpace(fileName) == "" || strings.ContainsRune(fileName, 0) {
		return nil, &OpError{Op: "validate", Err: fs.ErrInvalid}
	}
	select {
	case c.slots <- struct{}{}:
		cleanups = append(cleanups, func() { <-c.slots })
	case <-ctx.Done():
		return nil, opError(ctx, "admit", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, opError(ctx, "admit", err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.config.Host, strconv.Itoa(c.config.Port)))
	if err != nil {
		return nil, opError(ctx, "connect", err)
	}
	cleanups = append(cleanups, func() { _ = conn.Close() })
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, opError(ctx, "deadline", err)
	}
	stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	dialer := smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: c.config.Username, Password: c.config.Password, Domain: c.config.Domain}}
	session, err := dialer.DialConn(ctx, conn, c.config.Host)
	if err != nil {
		return nil, opError(ctx, "authenticate", err)
	}
	session = session.WithContext(ctx)
	cleanups = append(cleanups, func() { _ = session.Logoff() })
	share, err := session.Mount(shareName)
	if err != nil {
		return nil, opError(ctx, "mount", err)
	}
	share = share.WithContext(ctx)
	cleanups = append(cleanups, func() { _ = share.Umount() })
	file, err := share.Open(fileName)
	if err != nil {
		return nil, opError(ctx, "open", err)
	}
	cleanups = append(cleanups, func() { _ = file.Close() })
	info, err := file.Stat()
	if err != nil {
		return nil, opError(ctx, "stat", err)
	}
	if info.Size() > c.config.MaxFileBytes {
		return nil, &OpError{Op: "open", Err: ErrTooLarge}
	}
	if info.IsDir() {
		return nil, &OpError{Op: "open", Err: fs.ErrInvalid}
	}
	stream := newStream(ctx, cancel, file, info, c.config.MaxFileBytes, func() { _ = conn.Close() }, func() {
		for i := len(cleanups) - 2; i >= 0; i-- {
			cleanups[i]()
		}
	})
	transferred = true
	context.AfterFunc(ctx, func() { _ = stream.Close() })
	if err := ctx.Err(); err != nil {
		_ = stream.Close()
		return nil, opError(ctx, "open", err)
	}
	return stream, nil
}

type statReader interface {
	io.Reader
	Stat() (fs.FileInfo, error)
}

// Reject a known oversized file before reading, then enforce the actual bytes
// too: a remote file can grow after Stat or report an inaccurate length.
func readBounded(file statReader, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
