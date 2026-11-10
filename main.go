package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(".env", os.LookupEnv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	requests, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, err := newSMBClient(cfg)
	if err != nil {
		return fmt.Errorf("SMB client configuration: %w", err)
	}
	app := newApp(cfg, reader, requests)
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	return serve(ctx, app, listener, cfg.ShutdownTimeout, cancel)
}

// Stop accepting traffic first; cancel remaining SMB work after the grace period.
func serve(ctx context.Context, app *fiber.App, listener net.Listener, grace time.Duration, cancelRequests context.CancelFunc) error {
	defer listener.Close()
	defer cancelRequests()
	done := make(chan error, 1)
	ready := &readyListener{Listener: listener, started: make(chan struct{})}
	go func() { done <- app.Listener(ready) }()
	// Accept is reached only after fasthttp registers the listener. This also
	// handles a SIGTERM arriving immediately after startup without racing shutdown.
	select {
	case <-ready.started:
	case err := <-done:
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if err := app.ShutdownWithTimeout(grace); err != nil {
			cancelRequests()
			return fmt.Errorf("HTTP shutdown exceeded grace period: %w", err)
		}
		return <-done
	}
}

type readyListener struct {
	net.Listener
	started chan struct{}
	once    sync.Once
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.started) })
	return l.Listener.Accept()
}
