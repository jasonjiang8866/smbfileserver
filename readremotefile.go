package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/hirochachacha/go-smb2"
)

// FileReader is the narrow boundary shared by production routes and their tests.
// Implementations must honor cancellation and support concurrent calls.
type FileReader interface {
	ReadFile(context.Context, string, string) ([]byte, error)
}

type SMBReader struct{ config Config }

func (r *SMBReader) ReadFile(parent context.Context, shareName, fileName string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, r.config.RequestTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(r.config.SMBHost, r.config.SMBPort))
	if err != nil {
		return nil, fmt.Errorf("connect SMB: %w", err)
	}
	// Register cleanup immediately: authentication and mounting may both fail.
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set SMB deadline: %w", err)
	}
	// Cancellation must also interrupt transport I/O, including stalled cleanup.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	dialer := smb2.Dialer{Initiator: &smb2.NTLMInitiator{
		User: r.config.SMBUser, Password: r.config.SMBPassword, Domain: r.config.SMBDomain,
	}}
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("authenticate SMB: %w", smbError(ctx, err))
	}
	session = session.WithContext(ctx)
	defer session.Logoff()
	share, err := session.Mount(shareName)
	if err != nil {
		return nil, fmt.Errorf("mount SMB share: %w", smbError(ctx, err))
	}
	share = share.WithContext(ctx)
	defer share.Umount()
	data, err := share.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("read SMB file: %w", smbError(ctx, err))
	}
	return data, nil
}

// The pinned SMB library's context/transport wrappers predate errors.Unwrap.
func smbError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ce *smb2.ContextError
	if errors.As(err, &ce) {
		return ce.Err
	}
	var te *smb2.TransportError
	if errors.As(err, &te) {
		return te.Err
	}
	return err
}
