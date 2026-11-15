package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/jasonjiang8866/smbfileserver/access"
)

// Load before binding a listener. Never include file contents or parser errors
// in diagnostics: they may contain secrets even in an incorrectly shaped file.
func loadAccessPolicy(path string, backend access.Reader) (*access.Policy, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open ACCESS_CONFIG_FILE")
	}
	defer file.Close()
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return nil, errors.New("ACCESS_CONFIG_FILE unreadable or exceeds 1 MiB")
	}
	var cfg access.Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, errors.New("invalid ACCESS_CONFIG_FILE JSON/schema")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("ACCESS_CONFIG_FILE must contain one JSON object")
	}
	policy, err := access.New(cfg, backend)
	if err != nil {
		return nil, errors.New("invalid ACCESS_CONFIG_FILE policy")
	}
	return policy, nil
}
