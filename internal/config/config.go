// Package config loads and persists the daemon/CLI state.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultPort is the daemon's HTTP listen port.
const DefaultPort = 8787

// Config is the persisted state, shared by the daemon and CLI.
// It is plain data; callers that mutate it concurrently must serialize access
// (the daemon holds a mutex around it).
type Config struct {
	Token       string `json:"token"`        // shared secret across the fleet
	Target      string `json:"target"`       // active target peer name ("" = none)
	SyncEnabled bool   `json:"sync_enabled"` // watcher pushes when true
	RecvDir     string `json:"recv_dir"`     // where received files are saved
	Port        int    `json:"port"`         // HTTP listen port

	path string // where this config was loaded from
}

// Load reads the config, creating it with defaults on first run.
// If CLIPPY_CONFIG is set, that path is used (handy for running multiple
// daemons on one host during testing); otherwise the per-OS config dir.
func Load() (*Config, error) {
	p := os.Getenv("CLIPPY_CONFIG")
	if p == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		p = filepath.Join(base, "clippy", "config.json")
	}
	return loadPath(p)
}

func loadPath(p string) (*Config, error) {
	c := &Config{path: p}
	data, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// First run: seed defaults and persist.
		c.Token = newToken()
		c.RecvDir = defaultRecvDir()
		c.Port = DefaultPort
		c.SyncEnabled = true
		if err := c.Save(); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	c.path = p
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.RecvDir == "" {
		c.RecvDir = defaultRecvDir()
	}
	return c, nil
}

// Save writes the config back to disk (0600, dir 0700).
func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}

// Path returns the file this config is persisted to.
func (c *Config) Path() string { return c.path }

func defaultRecvDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "clippy-inbox"
	}
	return filepath.Join(home, "Downloads", "clippy")
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "changeme-insecure-token"
	}
	return hex.EncodeToString(b)
}
