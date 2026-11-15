package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
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
	backend, err := newSMBClient(cfg)
	if err != nil {
		return fmt.Errorf("SMB client configuration: %w", err)
	}
	reader, err := loadAccessPolicy(cfg.AccessConfigFile, backend)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	if cfg.HTTPServer == "fiber" {
		return serve(ctx, newApp(cfg, reader, requests), listener, cfg.ShutdownTimeout, cancel)
	}
	handler, err := newHTTPHandler(cfg, reader)
	if err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: cfg.RequestTimeout + 5*time.Second, IdleTimeout: 60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return requests }}
	return serveHTTP(ctx, server, listener, cfg.ShutdownTimeout, cancel)
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
