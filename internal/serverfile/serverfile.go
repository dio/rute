//go:build unix

// Package serverfile records where a running rute listens, so launchers can
// find a server that chose its ports at startup, and keeps one server per
// state directory.
package serverfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Info describes a running rute.
type Info struct {
	PID      int       `json:"pid"`
	Version  string    `json:"version"`
	Started  time.Time `json:"started"`
	ProxyURL string    `json:"proxy_url"`
	AdminURL string    `json:"admin_url"`
}

// ErrRunning reports that another rute holds the state directory.
var ErrRunning = errors.New("another rute is already running with this state directory")

// Lock takes the state directory's server lock without waiting. The lock is
// released when the returned file is closed or the process exits.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, fmt.Errorf("serverfile: lock: %w", err)
	}
	return file, nil
}

// Write replaces the file at path with info, readable only by the owner.
func Write(path string, info Info) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".server-*.json")
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
	return os.Rename(temp.Name(), path)
}

// Read returns the info at path.
func Read(path string) (Info, error) {
	var info Info
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, fmt.Errorf("serverfile: %s: %w", path, err)
	}
	return info, nil
}

// Remove deletes the file at path when it still describes process pid.
func Remove(path string, pid int) error {
	info, err := Read(path)
	if err != nil || info.PID != pid {
		return nil
	}
	return os.Remove(path)
}
