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
	"syscall"
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

func TestAdminStopAndReloadKeepAddresses(t *testing.T) {
	stateDir := t.TempDir()
	serverFile := filepath.Join(stateDir, "server.json")
	var current context.CancelFunc
	start := func(listen, adminListen string) (<-chan error, string, error) {
		ctx, cancel := context.WithCancel(context.Background())
		current = cancel
		exited := make(chan error, 1)
		go func() {
			exited <- runServe(ctx, []string{
				"-listen=" + listen, "-admin-listen=" + adminListen, "-credential-store=memory",
				"-metrics-db=off", "-routing-file=off", "-server-file=" + serverFile, "-initial-login=false",
			}, io.Discard, func(string) string { return "" })
		}()
		return exited, filepath.Join(stateDir, "rute.log"), nil
	}
	launcher := &launcher{
		stateDir: stateDir,
		candidates: func() []string {
			if info, err := serverfile.Read(serverFile); err == nil {
				return []string{info.AdminURL}
			}
			return nil
		},
		stderr: io.Discard,
		client: &http.Client{Timeout: 5 * time.Second},
		spawn:  start,
		signal: func(pid int, _ syscall.Signal) error {
			if pid != os.Getpid() {
				t.Errorf("signalled process %d, want %d", pid, os.Getpid())
			}
			current()
			return nil
		},
		poll: 10 * time.Millisecond,
	}
	ctx := context.Background()
	if _, err := launcher.stopServer(ctx); !errors.Is(err, errNotRunning) {
		t.Fatalf("stop with no server = %v", err)
	}
	if _, err := launcher.ensureServer(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	first, err := serverfile.Read(serverFile)
	if err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	if err := launcher.printStatus(ctx, &stdout); err != nil || !strings.Contains(stdout.String(), first.ProxyURL) {
		t.Errorf("status = %q, %v", stdout.String(), err)
	}

	detach := launcher.attach()
	if err := launcher.reload(ctx, &stdout); err != nil {
		t.Fatalf("reload: %v", err)
	}
	second, err := serverfile.Read(serverFile)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProxyURL != first.ProxyURL || second.AdminURL != first.AdminURL || !second.Started.After(first.Started) {
		t.Errorf("reload moved the server: %+v -> %+v", first, second)
	}
	detach()

	if _, err := launcher.stopServer(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := launcher.running(); !errors.Is(err, errNotRunning) {
		t.Errorf("running after stop = %v", err)
	}
	if _, err := os.Stat(serverFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("server file left after stop: %v", err)
	}
}
