//go:build unix

package serverfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockWriteReadRemove(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "rute")
	lock, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrRunning) {
		t.Fatalf("second lock = %v, want ErrRunning", err)
	}
	path := filepath.Join(dir, "server.json")
	info := Info{PID: 42, Version: "test", Started: time.Now().UTC().Round(time.Second),
		ProxyURL: "http://127.0.0.1:50001", AdminURL: "http://127.0.0.1:50002"}
	if err := Write(path, info); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.Stat(path); err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", stat.Mode().Perm(), err)
	}
	got, err := Read(path)
	if err != nil || got != info {
		t.Fatalf("read = %+v, %v", got, err)
	}
	if err := Remove(path, 7); err != nil || !exists(path) {
		t.Fatal("removed another process's file")
	}
	if err := Remove(path, 42); err != nil || exists(path) {
		t.Fatal("did not remove own file")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	relock, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = relock.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
