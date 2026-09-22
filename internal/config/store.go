package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Store owns the live configuration and its on-disk representation.
//
// Readers get an immutable snapshot through Current; writers go through Save,
// which validates, persists, and publishes in that order. A rejected config
// therefore never reaches disk and never reaches a running request.
type Store struct {
	path    string
	mu      sync.Mutex
	current atomic.Pointer[Config]
}

// NewStore seeds a store with an already-validated config. The seed is
// normalized first so callers may hand over a partially filled or legacy
// (single-upstream) shape and still get a coherent snapshot.
func NewStore(path string, cfg *Config) *Store {
	store := &Store{path: path}
	if cfg == nil {
		cfg = &Config{}
	}
	seed := cfg.Clone()
	seed.Normalize()
	store.current.Store(seed)
	return store
}

// Path is where Save writes.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Current returns the live config.
//
// The pointer is shared with every in-flight request and must be treated as
// read-only; it is deliberately not cloned so the relay hot path stays
// allocation-free. Callers that need to modify a config must Clone it first.
func (s *Store) Current() *Config {
	if s == nil {
		return &Config{}
	}
	if cfg := s.current.Load(); cfg != nil {
		return cfg
	}
	return &Config{}
}

// Save normalises and validates cfg, writes it to disk, then publishes it.
func (s *Store) Save(cfg *Config) error {
	if s == nil {
		return fmt.Errorf("config store is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := cfg.Clone()
	candidate.Normalize()
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := writeFile(s.path, candidate); err != nil {
		return err
	}
	s.current.Store(candidate)
	return nil
}

// writeFile persists cfg atomically: a crash mid-write leaves the previous
// config intact rather than a truncated file.
func writeFile(path string, cfg *Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tempName := temp.Name()

	defer func() {
		if tempName != "" {
			_ = os.Remove(tempName)
		}
	}()

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("flush temp config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}

	// Windows refuses to rename onto an existing file, so drop the target
	// first. The temp file already holds the complete new contents.
	if err := os.Rename(tempName, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replace config: %w (after %v)", removeErr, err)
		}
		if err := os.Rename(tempName, path); err != nil {
			return fmt.Errorf("replace config: %w", err)
		}
	}
	tempName = ""
	return nil
}
