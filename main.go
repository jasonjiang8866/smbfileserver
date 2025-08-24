package main

import (
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/joho/godotenv"
)

// FileRequest represents a request for a single file
type FileRequest struct {
	FileName string `json:"fileName"`
}

// BatchFileRequest represents a batch request for multiple files
type BatchFileRequest struct {
	Files       []FileRequest `json:"files"`
	Concurrency int           `json:"concurrency,omitempty"`
}

// FileResponse represents the response for a single file
type FileResponse struct {
	FileName string `json:"fileName"`
	Content  string `json:"content"`
	Error    string `json:"error,omitempty"`
}

// BatchFileResponse represents the response for multiple files
type BatchFileResponse struct {
	Files []FileResponse `json:"files"`
}

// fetchFileWorker handles concurrent file fetching
func fetchFileWorker(user User, shareName string, fileRequests <-chan FileRequest, results chan<- FileResponse, wg *sync.WaitGroup) {
	defer wg.Done()
	
	for fileReq := range fileRequests {
		session, conn := connectSMBserver(user)
		mount := getMount(session, shareName)
		
		var response FileResponse
		response.FileName = fileReq.FileName
		
		func() {
			defer session.Logoff()
			defer conn.Close()
			defer mount.Umount()
			
			fileBytes, err := readFileWithError(mount, fileReq.FileName)
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Content = base64.StdEncoding.EncodeToString(fileBytes)
			}
		}()
		
		results <- response
	}
}

func main() {
	app := fiber.New()

	app.Use(logger.New())

	api := app.Group("/api")

	v1 := api.Group("/v1")

	v1.Get("/hello", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World 👋!")
	})

	v1.Get("/smb/:shareName/:fileName", func(c *fiber.Ctx) error {
		// use godotenv to get env variables
		err := godotenv.Load(".env")
		if err != nil {
			panic(err)
		}
		// create user struct
		user := User{
			serverName:   os.Getenv("serverName"),
			serverIP:     os.Getenv("serverIP"),
			userName:     os.Getenv("userName"),
			userPassword: os.Getenv("userPassword"),
		}

		session, conn := connectSMBserver(user) // get smb session
		defer session.Logoff()
		defer conn.Close()
		shareName := c.Params("shareName")
		fileName := c.Params("fileName")

		mount := getMount(session, shareName) // get share
		defer mount.Umount()
		fileBytes := readFile(mount, fileName) // read file
		//change header to IMAGE file
		c.Set("Content-Type", "image/jpg, image/png, image/jpeg")
		return c.Send(fileBytes)
	})

	// New concurrent endpoint for batch file processing
	v1.Post("/smb/:shareName/batch", func(c *fiber.Ctx) error {
		// use godotenv to get env variables
		err := godotenv.Load(".env")
		if err != nil {
			panic(err)
		}
		
		// create user struct
		user := User{
			serverName:   os.Getenv("serverName"),
			serverIP:     os.Getenv("serverIP"),
			userName:     os.Getenv("userName"),
			userPassword: os.Getenv("userPassword"),
		}

		shareName := c.Params("shareName")
		
		// Parse request body
		var batchReq BatchFileRequest
		if err := c.BodyParser(&batchReq); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}
		
		// Default concurrency to 3 if not specified or invalid
		concurrency := batchReq.Concurrency
		if concurrency <= 0 {
			concurrency = 3
		}
		// Limit maximum concurrency to prevent resource exhaustion
		if concurrency > 10 {
			concurrency = 10
		}
		
		// Check if we have files to process
		if len(batchReq.Files) == 0 {
			return c.Status(400).JSON(fiber.Map{
				"error": "No files specified",
			})
		}
		
		// Set up channels and worker pool
		fileRequests := make(chan FileRequest, len(batchReq.Files))
		results := make(chan FileResponse, len(batchReq.Files))
		
		var wg sync.WaitGroup
		
		// Start worker goroutines
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go fetchFileWorker(user, shareName, fileRequests, results, &wg)
		}
		
		// Send file requests to workers
		go func() {
			for _, fileReq := range batchReq.Files {
				fileRequests <- fileReq
			}
			close(fileRequests)
		}()
		
		// Wait for all workers to complete and close results channel
		go func() {
			wg.Wait()
			close(results)
		}()
		
		// Collect results
		var responses []FileResponse
		for response := range results {
			responses = append(responses, response)
		}
		
		// Return batch response
		return c.JSON(BatchFileResponse{
			Files: responses,
		})
	})

	// Alternative GET endpoint for batch processing with query parameters
	v1.Get("/smb/:shareName/batch", func(c *fiber.Ctx) error {
		// use godotenv to get env variables
		err := godotenv.Load(".env")
		if err != nil {
			panic(err)
		}
		
		// create user struct
		user := User{
			serverName:   os.Getenv("serverName"),
			serverIP:     os.Getenv("serverIP"),
			userName:     os.Getenv("userName"),
			userPassword: os.Getenv("userPassword"),
		}

		shareName := c.Params("shareName")
		
		// Parse query parameters
		filesParam := c.Query("files")
		concurrencyParam := c.Query("concurrency", "3")
		
		if filesParam == "" {
			return c.Status(400).JSON(fiber.Map{
				"error": "No files specified. Use ?files=file1.jpg,file2.png",
			})
		}
		
		// Parse concurrency
		concurrency, err := strconv.Atoi(concurrencyParam)
		if err != nil || concurrency <= 0 {
			concurrency = 3
		}
		// Limit maximum concurrency
		if concurrency > 10 {
			concurrency = 10
		}
		
		// Parse file names (comma-separated)
		fileNames := []string{}
		for _, fileName := range strings.Split(filesParam, ",") {
			fileName = strings.TrimSpace(fileName)
			if fileName != "" {
				fileNames = append(fileNames, fileName)
			}
		}
		
		if len(fileNames) == 0 {
			return c.Status(400).JSON(fiber.Map{
				"error": "No valid file names provided",
			})
		}
		
		// Convert to FileRequest structs
		var fileRequests []FileRequest
		for _, fileName := range fileNames {
			fileRequests = append(fileRequests, FileRequest{FileName: fileName})
		}
		
		// Set up channels and worker pool
		fileRequestChan := make(chan FileRequest, len(fileRequests))
		results := make(chan FileResponse, len(fileRequests))
		
		var wg sync.WaitGroup
		
		// Start worker goroutines
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go fetchFileWorker(user, shareName, fileRequestChan, results, &wg)
		}
		
		// Send file requests to workers
		go func() {
			for _, fileReq := range fileRequests {
				fileRequestChan <- fileReq
			}
			close(fileRequestChan)
		}()
		
		// Wait for all workers to complete and close results channel
		go func() {
			wg.Wait()
			close(results)
		}()
		
		// Collect results
		var responses []FileResponse
		for response := range results {
			responses = append(responses, response)
		}
		
		// Return batch response
		return c.JSON(BatchFileResponse{
			Files: responses,
		})
	})

	app.Listen(":3000")

}
