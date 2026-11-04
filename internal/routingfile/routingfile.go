// Package routingfile stores routing chosen in the management UI, so a rute
// started on demand keeps it.
package routingfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Routing is one provider's balance mode, account order, and per-account
// in-flight limit.
type Routing struct {
	Balance     string   `json:"balance"`
	Order       []string `json:"order,omitempty"`
	MaxInFlight int      `json:"max_in_flight"`
}

// File reads and writes routing by provider. It is safe for concurrent use
// within one process.
type File struct {
	path string
	mu   sync.Mutex
}

// New returns a File at path.
func New(path string) *File {
	return &File{path: path}
}

// Path returns the file's path.
func (file *File) Path() string {
	return file.path
}

// Load returns the saved routing. A missing file has none.
func (file *File) Load() (map[string]Routing, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	return file.load()
}

func (file *File) load() (map[string]Routing, error) {
	data, err := os.ReadFile(file.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Routing{}, nil
	}
	if err != nil {
		return nil, err
	}
	routing := map[string]Routing{}
	if err := json.Unmarshal(data, &routing); err != nil {
		return nil, fmt.Errorf("routingfile: %s: %w", file.path, err)
	}
	return routing, nil
}

// Save replaces provider's routing and keeps the others.
func (file *File) Save(provider string, routing Routing) error {
	file.mu.Lock()
	defer file.mu.Unlock()
	all, err := file.load()
	if err != nil {
		return err
	}
	all[provider] = routing
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file.path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file.path), ".routing-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file.path)
}
