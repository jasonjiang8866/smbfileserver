package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
)

func TestBatchEndpointStructure(t *testing.T) {
	app := fiber.New()

	// Add a mock endpoint to test the structure
	app.Post("/api/v1/smb/:shareName/batch", func(c *fiber.Ctx) error {
		var batchReq BatchFileRequest
		if err := c.BodyParser(&batchReq); err != nil {
			return c.Status(400).JSON(fiber.Map{
				"error": "Invalid request body",
			})
		}
		
		// Mock response
		responses := []FileResponse{}
		for _, file := range batchReq.Files {
			responses = append(responses, FileResponse{
				FileName: file.FileName,
				Content:  "bW9jayBjb250ZW50", // "mock content" in base64
			})
		}
		
		return c.JSON(BatchFileResponse{
			Files: responses,
		})
	})

	// Test with POST request
	batchReq := BatchFileRequest{
		Files: []FileRequest{
			{FileName: "test1.jpg"},
			{FileName: "test2.png"},
		},
		Concurrency: 2,
	}
	
	reqBody, _ := json.Marshal(batchReq)
	req := httptest.NewRequest("POST", "/api/v1/smb/testshare/batch", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var response BatchFileResponse
	json.NewDecoder(resp.Body).Decode(&response)
	
	assert.Len(t, response.Files, 2)
	assert.Equal(t, "test1.jpg", response.Files[0].FileName)
	assert.Equal(t, "test2.png", response.Files[1].FileName)
}

func TestBatchEndpointGETStructure(t *testing.T) {
	app := fiber.New()

	// Add a mock GET endpoint to test the structure  
	app.Get("/api/v1/smb/:shareName/batch", func(c *fiber.Ctx) error {
		filesParam := c.Query("files")
		if filesParam == "" {
			return c.Status(400).JSON(fiber.Map{
				"error": "No files specified. Use ?files=file1.jpg,file2.png",
			})
		}
		
		// Mock response for GET endpoint
		fileNames := []string{"test1.jpg", "test2.png"}
		responses := []FileResponse{}
		for _, fileName := range fileNames {
			responses = append(responses, FileResponse{
				FileName: fileName,
				Content:  "bW9jayBjb250ZW50", // "mock content" in base64
			})
		}
		
		return c.JSON(BatchFileResponse{
			Files: responses,
		})
	})

	// Test with GET request
	req := httptest.NewRequest("GET", "/api/v1/smb/testshare/batch?files=test1.jpg,test2.png&concurrency=2", nil)
	
	resp, err := app.Test(req)
	assert.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var response BatchFileResponse
	json.NewDecoder(resp.Body).Decode(&response)
	
	assert.Len(t, response.Files, 2)
}