package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	recoverer "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

func newApp(cfg Config, reader smbhttp.FileReader, root context.Context) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ReadTimeout:           10 * time.Second,
		WriteTimeout:          cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:           60 * time.Second,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			status, message := http.StatusInternalServerError, "internal server error"
			var fe *fiber.Error
			if errors.As(err, &fe) {
				status = fe.Code
				message = http.StatusText(status)
			}
			return c.Status(status).JSON(fiber.Map{"error": message})
		},
	})
	app.Use(recoverer.New())
	api := app.Group("/api/v1")
	api.Get("/hello", func(c *fiber.Ctx) error { return c.SendString("Hello, World 👋!") })

	handler, err := smbhttp.New(reader, smbhttp.Options{RequestTimeout: cfg.RequestTimeout, DisableStreaming: true, MaxBatchFiles: cfg.MaxBatchFiles, MaxBatchBytes: cfg.MaxBatchBytes})
	if err != nil {
		panic(err)
	} // Gateway configuration is validated before startup.
	mounted := http.StripPrefix("/api/v1/smb", handler)
	// fasthttp RequestCtx is pooled. Deriving a Go cancelCtx from it can leave
	// cancellation propagation reading its values after Fiber recycles it.
	// Fiber has no per-request disconnect signal; use the application-owned root.
	bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mounted.ServeHTTP(w, r.WithContext(root))
	})
	app.Use("/api/v1/smb", adaptor.HTTPHandler(bridge))
	return app
}

// The native server is the default gateway path and preserves disconnect context.
func newHTTPHandler(cfg Config, reader smbhttp.FileReader) (http.Handler, error) {
	handler, err := smbhttp.New(reader, smbhttp.Options{RequestTimeout: cfg.RequestTimeout, MaxBatchFiles: cfg.MaxBatchFiles, MaxBatchBytes: cfg.MaxBatchBytes})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/hello", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("Hello, World 👋!")) })
	mux.Handle("/api/v1/smb/", http.StripPrefix("/api/v1/smb", handler))
	return mux, nil
}
