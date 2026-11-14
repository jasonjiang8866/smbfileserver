//go:build integration

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jasonjiang8866/smbfileserver/smbhttp"
)

func TestSMBIntegrationAuthenticatedGateway(t *testing.T) {
	cfg := integrationConfig(t)
	reader := testSMBClient(t, cfg)
	p, err := loadAccessPolicy(writeTestPolicy(t), reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native", "fiber"} {
		t.Run(mode, func(t *testing.T) {
			h, err := newHTTPHandler(cfg, p)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(h)
			defer server.Close()
			app := newApp(cfg, p, context.Background())
			invoke := func(method, path, body, token string) (int, []byte, http.Header) {
				t.Helper()
				r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Content-Type", "application/json")
				if token != "" {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				var response *http.Response
				if mode == "fiber" {
					response, err = app.Test(r, 5000)
				} else {
					response, err = server.Client().Do(r)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				return response.StatusCode, data, response.Header
			}
			for _, tc := range []struct {
				path, token string
				status      int
			}{
				{"/api/v1/smb/reports/hello.txt", "", 401},
				{"/api/v1/smb/reports/hello.txt", gatewayTestToken, 200},
				{"/api/v1/smb/readonly/hello.txt", gatewayTestToken, 403},
				{"/api/v1/smb/blocked/hello.txt", gatewayTestToken, 403},
				{"/api/v1/smb/reports/..%5coutside-secret.txt", gatewayTestToken, 400},
			} {
				status, data, headers := invoke("GET", tc.path, "", tc.token)
				if status != tc.status {
					t.Fatalf("%s: status=%d body=%s", tc.path, status, data)
				}
				if tc.status == 200 && string(data) != "SMB integration fixture\n" {
					t.Fatal("wrong file")
				}
				if headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Disposition") != "attachment" {
					t.Fatal("missing response policy")
				}
			}
			status, data, _ := invoke("POST", "/api/v1/smb/reports/batch", `{"files":[{"fileName":"nested/工艺 报表.csv"},{"fileName":"../outside-secret.txt"},{"fileName":"file:stream"}]}`, gatewayTestToken)
			var batch smbhttp.BatchFileResponse
			if status != 200 || json.Unmarshal(data, &batch) != nil || len(batch.Files) != 3 {
				t.Fatalf("batch %d %s", status, data)
			}
			decoded, err := base64.StdEncoding.DecodeString(batch.Files[0].Content)
			if err != nil || string(decoded) != "lot,value\n晶圆,42\n" {
				t.Fatalf("nested read: %s", data)
			}
			for _, file := range batch.Files[1:] {
				if file.Error != "invalid file or share name" {
					t.Fatal(file)
				}
			}
			// This is a server-enforced boundary: the fixture explicitly disables
			// symlink following and wide links. Lexical validation alone cannot do this.
			status, data, _ = invoke("POST", "/api/v1/smb/reports/batch", `{"files":[{"fileName":"escape-link.txt"},{"fileName":"escape-dir/outside-secret.txt"}]}`, gatewayTestToken)
			if status != 200 || json.Unmarshal(data, &batch) != nil || len(batch.Files) != 2 {
				t.Fatal(string(data))
			}
			for _, file := range batch.Files {
				if file.Error == "" || file.Content != "" {
					t.Fatalf("server link boundary failed: %+v", file)
				}
			}
			status, data, headers := invoke("HEAD", "/api/v1/smb/reports/hello.txt", "", gatewayTestToken)
			if status != 200 || len(data) != 0 || headers.Get("Content-Length") != "24" {
				t.Fatalf("HEAD %d %v", status, headers)
			}
		})
	}
}
