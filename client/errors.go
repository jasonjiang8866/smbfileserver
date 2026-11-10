package client

import (
	"context"
	"errors"
	"io/fs"
	"net"

	"github.com/cloudsoda/go-smb2"
)

// ErrUnavailable indicates a transport, authentication or protocol failure not
// covered by a more specific standard error. It does not imply retry safety.
var ErrUnavailable = errors.New("SMB operation failed")

// ErrTooLarge means the file exceeds Config.MaxFileBytes. Use errors.Is.
// Its SizeLimitExceeded marker lets HTTP adapters recognize it without importing
// this package or the SMB backend.
var ErrTooLarge error = &sizeLimitError{}

type sizeLimitError struct{}

func (*sizeLimitError) Error() string           { return "file size limit exceeded" }
func (*sizeLimitError) SizeLimitExceeded() bool { return true }

// OpError identifies the failing operation without including credentials, host,
// filename or raw backend diagnostics. Err is a backend-independent category:
// fs.ErrInvalid, fs.ErrNotExist, fs.ErrPermission, context.Canceled,
// context.DeadlineExceeded, ErrTooLarge or ErrUnavailable.
type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string { return "SMB " + e.Op + ": " + e.Err.Error() }
func (e *OpError) Unwrap() error { return e.Err }

func opError(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return &OpError{Op: op, Err: ctx.Err()}
	}
	// CloudSoda's transport wrapper does not implement Unwrap.
	var transport *smb2.TransportError
	if errors.As(err, &transport) {
		err = transport.Err
	}
	var network net.Error
	kind := ErrUnavailable
	switch {
	case errors.Is(err, context.Canceled):
		kind = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		kind = context.DeadlineExceeded
	case errors.As(err, &network) && network.Timeout():
		kind = context.DeadlineExceeded
	case errors.Is(err, ErrTooLarge):
		kind = ErrTooLarge
	case errors.Is(err, fs.ErrNotExist):
		kind = fs.ErrNotExist
	case errors.Is(err, fs.ErrPermission):
		kind = fs.ErrPermission
	case errors.Is(err, fs.ErrClosed):
		kind = fs.ErrClosed
	case errors.Is(err, fs.ErrInvalid):
		kind = fs.ErrInvalid
	}
	return &OpError{Op: op, Err: kind}
}
