package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/serverfile"
)

func TestStorageDefaultsToStoreWithAccounts(t *testing.T) {
	for name, test := range map[string]struct {
		files []string
		path  string
		want  string
	}{
		"fresh install":      {want: "keyring"},
		"credential file":    {files: []string{"credentials.json"}, want: "file"},
		"keyring manifest":   {files: []string{"accounts.json", "credentials.json"}, want: "keyring"},
		"explicit file path": {path: "/elsewhere/credentials.json", want: "file"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
			stateDir, err := accountstore.DefaultStateDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, file := range test.files {
				if err := os.WriteFile(filepath.Join(stateDir, file), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			kind := ""
			if err := resolveStorageOptions(&kind, test.path); err != nil {
				t.Fatal(err)
			}
			if kind != test.want {
				t.Errorf("store = %q, want %q", kind, test.want)
			}
		})
	}
}

func TestServeStopsWhenIdleAndStaysUpWhileAttached(t *testing.T) {
	previous := idleCheckInterval
	idleCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { idleCheckInterval = previous })

	serverFile := filepath.Join(t.TempDir(), "server.json")
	args := []string{
		"-listen=127.0.0.1:0", "-admin-listen=127.0.0.1:0", "-credential-store=memory",
		"-metrics-db=off", "-routing-file=off", "-server-file=" + serverFile,
		"-initial-login=false", "-idle-timeout=300ms",
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runServe(context.Background(), args, writer, func(string) string { return "" })
		_ = writer.Close()
	}()
	lines := bufio.NewScanner(reader)
	var adminURL string
	for adminURL == "" && lines.Scan() {
		if address, ok := strings.CutPrefix(lines.Text(), "rute management on "); ok {
			adminURL = address
		}
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()
	if adminURL == "" {
		t.Fatal("serve did not report its management address")
	}
	info, err := serverfile.Read(serverFile)
	if err != nil || info.AdminURL != adminURL || info.PID != os.Getpid() || !strings.HasPrefix(info.ProxyURL, "http://127.0.0.1:") {
		t.Fatalf("server file = %+v, %v", info, err)
	}
	err = runServe(context.Background(), args, io.Discard, func(string) string { return "" })
	if !errors.Is(err, serverfile.ErrRunning) || !strings.Contains(err.Error(), adminURL) {
		t.Fatalf("second server = %v, want ErrRunning naming %s", err, adminURL)
	}

	attachCtx, detach := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(attachCtx, http.MethodGet, adminURL+"/v1/attach", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("serve stopped while a client was attached: %v", err)
	case <-time.After(time.Second):
	}
	detach()
	_ = response.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after the client detached")
	}
	if _, err := os.Stat(serverFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("server file left after stop: %v", err)
	}
}
