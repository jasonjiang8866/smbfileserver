package main

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// Bind before serving application traffic, so a metrics bind failure fails
// startup. Close on every application exit; scrapes do not delay SMB drain.
func startMetrics(address string, o *observability) (*http.Server, error) {
	server := &http.Server{Handler: http.HandlerFunc(o.metrics), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	if address == "" {
		return server, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("metrics listen: %w", err)
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			o.logger.Error("metrics_server_failed")
		}
	}()
	return server, nil
}
