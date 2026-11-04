// Package pricing estimates the API-equivalent cost of token usage from a
// user-maintained price list. Upstream responses carry token counts only, so
// costs are estimates.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// Rate is a model's price in US dollars per million tokens.
type Rate struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Tokens is the usage to price. Input excludes cached input.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Cost returns the cost of tokens at rate in US dollars.
func (rate Rate) Cost(tokens Tokens) float64 {
	return (float64(tokens.Input)*rate.Input +
		float64(tokens.Output)*rate.Output +
		float64(tokens.CacheRead)*rate.CacheRead +
		float64(tokens.CacheWrite)*rate.CacheWrite) / 1e6
}

// Table maps model names to rates. A name ending in "*" matches every model
// with that prefix; an exact name or the longest prefix wins.
type Table struct {
	Models map[string]Rate `json:"models"`
}

// Lookup returns the rate for model.
func (table Table) Lookup(model string) (Rate, bool) {
	if rate, ok := table.Models[model]; ok {
		return rate, true
	}
	var (
		best    Rate
		longest = -1
	)
	for pattern, rate := range table.Models {
		prefix, wildcard := strings.CutSuffix(pattern, "*")
		if wildcard && strings.HasPrefix(model, prefix) && len(prefix) > longest {
			best, longest = rate, len(prefix)
		}
	}
	return best, longest >= 0
}

// Parse decodes a price file.
func Parse(data []byte) (Table, error) {
	var table Table
	if err := json.Unmarshal(data, &table); err != nil {
		return Table{}, fmt.Errorf("pricing: %w", err)
	}
	for name, rate := range table.Models {
		if rate.Input < 0 || rate.Output < 0 || rate.CacheRead < 0 || rate.CacheWrite < 0 {
			return Table{}, fmt.Errorf("pricing: %s has a negative rate", name)
		}
	}
	return table, nil
}

// File reads a price file and reloads it when it changes. A missing file is
// an empty table. It is safe for concurrent use.
type File struct {
	path string

	mu       sync.Mutex
	modified time.Time
	size     int64
	table    Table
	err      error
}

// NewFile returns a File for path.
func NewFile(path string) *File {
	return &File{path: path}
}

// Path returns the file's path.
func (file *File) Path() string {
	return file.path
}

// Table returns the current table. When the file cannot be parsed it returns
// an empty table and the error.
func (file *File) Table() (Table, error) {
	info, err := os.Stat(file.path)
	file.mu.Lock()
	defer file.mu.Unlock()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		file.modified, file.size, file.table, file.err = time.Time{}, 0, Table{}, nil
		return file.table, nil
	case err != nil:
		return Table{}, err
	case info.ModTime().Equal(file.modified) && info.Size() == file.size:
		return file.table, file.err
	}
	file.modified, file.size = info.ModTime(), info.Size()
	data, err := os.ReadFile(file.path)
	if err == nil {
		file.table, err = Parse(data)
	}
	if err != nil {
		file.table = Table{}
	}
	file.err = err
	return file.table, err
}
