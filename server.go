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

func newApp(cfg Config, reader FileReader, root context.Context) *fiber.App {
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

	handler, err := smbhttp.New(reader, smbhttp.Options{RequestTimeout: cfg.RequestTimeout})
	if err != nil {
		panic(err)
	} // Gateway configuration is validated before startup.
	mounted := http.StripPrefix("/api/v1/smb", handler)
	// Preserve the gateway's graceful-shutdown cancellation independently of the
	// adapter's request context. Fiber v2 still cannot report client disconnects.
	bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(root, cancel)
		defer stop()
		defer cancel()
		if root.Err() != nil {
			cancel()
		}
		mounted.ServeHTTP(w, r.WithContext(ctx))
	})
	app.Use("/api/v1/smb", adaptor.HTTPHandler(bridge))
	return app
}
