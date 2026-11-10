package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	recoverer "github.com/gofiber/fiber/v2/middleware/recover"
)

type FileRequest struct {
	FileName string `json:"fileName"`
}
type BatchFileRequest struct {
	Files       []FileRequest `json:"files"`
	Concurrency int           `json:"concurrency,omitempty"`
}
type FileResponse struct {
	FileName string `json:"fileName"`
	Content  string `json:"content"`
	Error    string `json:"error,omitempty"`
}
type BatchFileResponse struct {
	Files []FileResponse `json:"files"`
}

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

	batch := func(c *fiber.Ctx) error {
		var req BatchFileRequest
		if c.Method() == http.MethodPost {
			if err := c.BodyParser(&req); err != nil {
				return fiber.NewError(400)
			}
		} else {
			req.Concurrency, _ = strconv.Atoi(c.Query("concurrency", "3"))
			for _, file := range strings.Split(c.Query("files"), ",") {
				if name := strings.TrimSpace(file); name != "" {
					req.Files = append(req.Files, FileRequest{FileName: name})
				}
			}
		}
		if len(req.Files) == 0 {
			return c.Status(400).JSON(fiber.Map{"error": "no files specified"})
		}
		for _, file := range req.Files {
			if strings.TrimSpace(file.FileName) == "" {
				return c.Status(400).JSON(fiber.Map{"error": "fileName must not be empty"})
			}
		}
		ctx, cancel := context.WithTimeout(root, cfg.RequestTimeout)
		defer cancel()
		return c.JSON(readBatch(ctx, reader, c.Params("shareName"), req))
	}
	// Register the literal path before :fileName; otherwise "batch" is a filename.
	api.Get("/smb/:shareName/batch", batch)
	api.Post("/smb/:shareName/batch", batch)
	api.Get("/smb/:shareName/:fileName", func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(root, cfg.RequestTimeout)
		defer cancel()
		data, err := reader.ReadFile(ctx, c.Params("shareName"), c.Params("fileName"))
		if err != nil {
			status, message := publicReadError(err)
			return c.Status(status).JSON(fiber.Map{"error": message})
		}
		c.Set("Content-Type", http.DetectContentType(data))
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Send(data)
	})
	return app
}

func readBatch(ctx context.Context, reader FileReader, share string, req BatchFileRequest) BatchFileResponse {
	workers := req.Concurrency
	if workers <= 0 {
		workers = 3
	}
	if workers > 10 {
		workers = 10
	}
	if workers > len(req.Files) {
		workers = len(req.Files)
	}
	response := BatchFileResponse{Files: make([]FileResponse, len(req.Files))}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				file := req.Files[index]
				result := FileResponse{FileName: file.FileName}
				data, err := readBatchFile(ctx, reader, share, file.FileName)
				if err != nil {
					_, result.Error = publicReadError(err)
				} else {
					result.Content = base64.StdEncoding.EncodeToString(data)
				}
				response.Files[index] = result
			}
		}()
	}
	// Each worker writes a distinct index; output order follows input order.
	for index := range req.Files {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return response
}

func readBatchFile(ctx context.Context, reader FileReader, share, file string) (data []byte, err error) {
	// HTTP recovery middleware cannot catch a panic in a separate worker goroutine.
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("file reader failed")
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return reader.ReadFile(ctx, share, file)
}

func publicReadError(err error) (int, string) {
	var networkError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "SMB request timed out"
	case errors.Is(err, context.Canceled):
		return 503, "request canceled"
	case errors.As(err, &networkError) && networkError.Timeout():
		return 504, "SMB request timed out"
	case errors.Is(err, fs.ErrNotExist):
		return 404, "file or share not found"
	case errors.Is(err, fs.ErrPermission):
		return 403, "file access denied"
	default:
		return 502, "SMB request failed"
	}
}
