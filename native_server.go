package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener, grace time.Duration, cancelRequests context.CancelFunc) error {
	defer cancelRequests()
	defer server.Close()
	defer listener.Close()
	done := make(chan error, 1)
	ready := &readyListener{Listener: listener, started: make(chan struct{})}
	go func() {
		err := server.Serve(ready)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case <-ready.started:
	case err := <-done:
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		deadline, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := server.Shutdown(deadline); err != nil {
			cancelRequests()
			_ = server.Close()
			<-done
			return fmt.Errorf("HTTP shutdown exceeded grace period: %w", err)
		}
		return <-done
	}
}
