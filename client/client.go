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
// method because every ReadFile closes its own resources before returning.
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

// ReadFile returns the whole file or nil on failure, never partial contents.
// It uses one total timeout, including time waiting for a concurrency slot; an
// earlier parent deadline wins. Canceled queued work does not dial the server.
// Each call establishes a fresh session: failures cannot poison another call,
// and there are no automatic retries or pooled connections.
//
// share must be a bare share name, not a UNC path. file may be a nested name.
// These checks are not authorization or path confinement; enforce those in the
// embedding application and on the SMB server using a read-only account.
func (c *Client) ReadFile(parent context.Context, shareName, fileName string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, c.config.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, opError(ctx, "admit", err)
	}
	if strings.TrimSpace(shareName) == "" || strings.ContainsAny(shareName, "/\\\x00") || strings.TrimSpace(fileName) == "" || strings.ContainsRune(fileName, 0) {
		return nil, &OpError{Op: "validate", Err: fs.ErrInvalid}
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, opError(ctx, "admit", ctx.Err())
	}
	// Both select cases can be ready when the parent is canceled.
	if err := ctx.Err(); err != nil {
		return nil, opError(ctx, "admit", err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.config.Host, strconv.Itoa(c.config.Port)))
	if err != nil {
		return nil, opError(ctx, "connect", err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, opError(ctx, "deadline", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	dialer := smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: c.config.Username, Password: c.config.Password, Domain: c.config.Domain}}
	session, err := dialer.DialConn(ctx, conn, c.config.Host)
	if err != nil {
		return nil, opError(ctx, "authenticate", err)
	}
	session = session.WithContext(ctx)
	defer session.Logoff()
	share, err := session.Mount(shareName)
	if err != nil {
		return nil, opError(ctx, "mount", err)
	}
	share = share.WithContext(ctx)
	defer share.Umount()
	file, err := share.Open(fileName)
	if err != nil {
		return nil, opError(ctx, "open", err)
	}
	defer file.Close()
	data, err := readBounded(file, c.config.MaxFileBytes)
	if err != nil {
		return nil, opError(ctx, "read", err)
	}
	return data, nil
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
