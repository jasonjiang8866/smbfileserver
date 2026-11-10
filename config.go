package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jasonjiang8866/smbfileserver/client"
	"github.com/joho/godotenv"
)

type Config struct {
	ListenAddress         string
	SMBHost               string
	SMBPort               string
	SMBUser               string
	SMBPassword           string
	SMBDomain             string
	RequestTimeout        time.Duration
	ShutdownTimeout       time.Duration
	SMBMaxConcurrentReads int
	SMBMaxFileBytes       int64
}

// Read an optional dotenv file once. Explicit environment values take precedence.
// Never include credential values (or dotenv parser errors) in diagnostics.
func loadConfig(envFile string, lookup func(string) (string, bool)) (Config, error) {
	values, err := godotenv.Read(envFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, errors.New("cannot read dotenv file; check its syntax and permissions")
	}
	get := func(key, legacy, fallback string) string {
		for _, name := range []string{key, legacy} {
			if name != "" {
				if value, ok := lookup(name); ok {
					return value
				}
			}
		}
		for _, name := range []string{key, legacy} {
			if value, ok := values[name]; name != "" && ok {
				return value
			}
		}
		return fallback
	}
	cfg := Config{
		ListenAddress: get("HTTP_ADDR", "", ":3000"),
		SMBHost:       strings.TrimSpace(get("SMB_HOST", "serverIP", "")),
		SMBPort:       get("SMB_PORT", "", "445"),
		SMBUser:       get("SMB_USERNAME", "userName", ""),
		SMBPassword:   get("SMB_PASSWORD", "userPassword", ""),
		SMBDomain:     get("SMB_DOMAIN", "", ""),
	}
	for _, field := range []struct{ name, value string }{
		{"SMB_HOST", cfg.SMBHost}, {"SMB_USERNAME", cfg.SMBUser}, {"SMB_PASSWORD", cfg.SMBPassword},
	} {
		if strings.TrimSpace(field.value) == "" {
			return Config{}, fmt.Errorf("%s is required", field.name)
		}
	}
	if strings.ContainsAny(cfg.SMBHost, "/\\ \t\r\n[]") || (strings.Contains(cfg.SMBHost, ":") && net.ParseIP(cfg.SMBHost) == nil) {
		return Config{}, errors.New("SMB_HOST must be a bare hostname or IP address; use SMB_PORT for the port")
	}
	if !validPort(cfg.SMBPort) {
		return Config{}, errors.New("SMB_PORT must be an integer between 1 and 65535")
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil || !validPort(port) {
		return Config{}, errors.New("HTTP_ADDR must be a host:port address with a port between 1 and 65535")
	}
	for _, field := range []struct {
		name, fallback string
		target         *time.Duration
	}{
		{"REQUEST_TIMEOUT", "30s", &cfg.RequestTimeout},
		{"SHUTDOWN_TIMEOUT", "15s", &cfg.ShutdownTimeout},
	} {
		d, err := time.ParseDuration(get(field.name, "", field.fallback))
		if err != nil || d <= 0 || d > 10*time.Minute {
			return Config{}, fmt.Errorf("%s must be a duration greater than zero and at most 10m", field.name)
		}
		*field.target = d
	}
	concurrency, err := strconv.Atoi(get("SMB_MAX_CONCURRENT_READS", "", strconv.Itoa(client.DefaultMaxConcurrentReads)))
	if err != nil || concurrency <= 0 {
		return Config{}, errors.New("SMB_MAX_CONCURRENT_READS must be a positive integer")
	}
	maxBytes, err := strconv.ParseInt(get("SMB_MAX_FILE_BYTES", "", strconv.FormatInt(client.DefaultMaxFileBytes, 10)), 10, 64)
	if err != nil || maxBytes <= 0 || maxBytes == 1<<63-1 {
		return Config{}, errors.New("SMB_MAX_FILE_BYTES must be a positive integer less than MaxInt64")
	}
	cfg.SMBMaxConcurrentReads, cfg.SMBMaxFileBytes = concurrency, maxBytes
	return cfg, nil
}

func validPort(value string) bool {
	p, err := strconv.Atoi(value)
	return err == nil && p > 0 && p <= 65535
}

// Construct exactly one client at startup so admission is shared by all requests.
func newSMBClient(cfg Config) (*client.Client, error) {
	port, err := strconv.Atoi(cfg.SMBPort)
	if err != nil {
		return nil, errors.New("SMB_PORT must be an integer")
	}
	return client.New(client.Config{Host: cfg.SMBHost, Port: port, Username: cfg.SMBUser,
		Password: cfg.SMBPassword, Domain: cfg.SMBDomain, Timeout: cfg.RequestTimeout,
		MaxConcurrentReads: cfg.SMBMaxConcurrentReads, MaxFileBytes: cfg.SMBMaxFileBytes})
}
