package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}
func validEnv() map[string]string {
	return map[string]string{"SMB_HOST": "files.example.test", "SMB_USERNAME": "reader", "SMB_PASSWORD": "secret"}
}
func TestConfigEnvironmentOnlyAndLegacy(t *testing.T) {
	for _, values := range []map[string]string{validEnv(), {"serverIP": "files.example.test", "userName": "reader", "userPassword": "secret"}} {
		cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.env"), envLookup(values))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SMBHost != "files.example.test" || cfg.SMBPassword != "secret" || cfg.RequestTimeout != 30*time.Second || cfg.ListenAddress != ":3000" {
			t.Fatalf("unexpected defaults (credentials omitted)")
		}
	}
}
func TestConfigPrecedenceAndSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("SMB_HOST=file-host\nSMB_USERNAME=file-user\nSMB_PASSWORD=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"serverIP": "env-host", "SMB_USERNAME": "new-user", "userName": "old-user"}
	cfg, err := loadConfig(path, envLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMBHost != "env-host" || cfg.SMBUser != "new-user" || cfg.SMBPassword != "file-secret" {
		t.Fatal("environment precedence failed")
	}
	values["SMB_USERNAME"] = "changed"
	if cfg.SMBUser != "new-user" {
		t.Fatal("config must be a startup snapshot")
	}
	values["SMB_PASSWORD"] = ""
	if _, err := loadConfig(path, envLookup(values)); err == nil {
		t.Fatal("explicit empty environment must not fall back to dotenv")
	}
}
func TestConfigValidation(t *testing.T) {
	cases := []struct{ key, value string }{
		{"SMB_HOST", ""}, {"SMB_USERNAME", ""}, {"SMB_PASSWORD", ""},
		{"SMB_HOST", "smb://host/share"}, {"SMB_HOST", "host:445"},
		{"SMB_PORT", "0"}, {"SMB_PORT", "65536"}, {"HTTP_ADDR", ":abc"},
		{"REQUEST_TIMEOUT", "0s"}, {"REQUEST_TIMEOUT", "-1s"}, {"REQUEST_TIMEOUT", "11m"},
		{"SHUTDOWN_TIMEOUT", "bad"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			values := validEnv()
			values[tc.key] = tc.value
			_, err := loadConfig(filepath.Join(t.TempDir(), "none"), envLookup(values))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("expected field-specific error, got %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("credential leaked")
			}
		})
	}
	values := validEnv()
	values["SMB_HOST"] = "::1"
	if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), envLookup(values)); err != nil {
		t.Fatal(err)
	}
}
func TestMalformedDotenvDoesNotLeakValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("SMB_PASSWORD=\"secret-never-log"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path, envLookup(validEnv()))
	if err == nil || strings.Contains(err.Error(), "secret-never-log") {
		t.Fatalf("expected sanitized parse error, got %v", err)
	}
}
