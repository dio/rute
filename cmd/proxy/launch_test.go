package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/rute/internal/admin"
	"github.com/dio/rute/internal/codexpool"
)

// fakeManagement serves the management endpoints the launcher uses.
type fakeManagement struct {
	t          *testing.T
	up         atomic.Bool
	attached   atomic.Int32
	mu         sync.Mutex
	accounts   []codexpool.AccountStatus
	proxyURL   string
	lastAPIKey string
	logins     int
	polls      int
}

func (fake *fakeManagement) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !fake.up.Load() {
		http.Error(writer, "down", http.StatusServiceUnavailable)
		return
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	switch {
	case request.URL.Path == "/v1/status":
		proxyURL := fake.proxyURL
		if proxyURL == "" {
			proxyURL = "http://127.0.0.1:20999"
		}
		_ = json.NewEncoder(writer).Encode(admin.StatusResponse{
			ProxyURL: proxyURL,
			Providers: []codexpool.Status{
				{Provider: "claude", Accounts: fake.accounts},
				{Provider: "codex", Accounts: fake.accounts},
				{Provider: "commandcode", Accounts: fake.accounts},
			},
		})
	case request.URL.Path == "/v1/logins" && request.Method == http.MethodPost:
		var body map[string]string
		_ = json.NewDecoder(request.Body).Decode(&body)
		if request.Header.Get("Content-Type") != "application/json" || body["name"] != "default" || body["action"] != "add" {
			fake.t.Errorf("login request %v, %q", body, request.Header.Get("Content-Type"))
		}
		fake.lastAPIKey = body["api_key"]
		fake.logins++
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(admin.Login{ID: "l1", State: admin.LoginPending,
			AuthorizationURL: "https://example.invalid/authorize"})
	case request.URL.Path == "/v1/logins/l1":
		fake.polls++
		state := admin.LoginPending
		if fake.polls >= 2 {
			state = admin.LoginDone
			fake.accounts = []codexpool.AccountStatus{{Name: "default"}}
		}
		_ = json.NewEncoder(writer).Encode(admin.Login{ID: "l1", State: state})
	case request.URL.Path == "/v1/attach":
		fake.attached.Add(1)
		defer fake.attached.Add(-1)
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		fake.mu.Unlock()
		<-request.Context().Done()
		fake.mu.Lock()
	default:
		http.NotFound(writer, request)
	}
}

type launchRun struct {
	name     string
	args     []string
	env      []string
	attached int32
}

func newTestLauncher(t *testing.T, fake *fakeManagement) (*launcher, *strings.Builder, *launchRun, *int) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	var stderr strings.Builder
	var ran launchRun
	spawned := 0
	launcher := &launcher{
		candidates: func() []string { return []string{server.URL} },
		stderr:     &stderr,
		client:     &http.Client{Timeout: 5 * time.Second},
		spawn: func(string, string) (<-chan error, string, error) {
			spawned++
			fake.up.Store(true)
			return make(chan error), "/state/rute.log", nil
		},
		openBrowser: func(url string) error {
			if url != "https://example.invalid/authorize" {
				t.Errorf("browser opened %q", url)
			}
			return nil
		},
		lookPath: func(name string) (string, error) { return "/bin/" + name, nil },
		run: func(_ context.Context, name string, args, env []string) error {
			deadline := time.Now().Add(2 * time.Second)
			for fake.attached.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			ran = launchRun{name: name, args: args, env: env, attached: fake.attached.Load()}
			return exitCodeError(3)
		},
		poll: time.Millisecond,
	}
	return launcher, &stderr, &ran, &spawned
}

func TestLaunchReusesRunningServer(t *testing.T) {
	t.Parallel()
	fake := &fakeManagement{t: t, accounts: []codexpool.AccountStatus{{Name: "default"}}}
	fake.up.Store(true)
	launcher, _, ran, spawned := newTestLauncher(t, fake)
	err := launcher.launch(context.Background(), "claude", []string{"--resume", "-p", "hi"})
	if code, ok := err.(exitCodeError); !ok || code != 3 {
		t.Fatalf("launch error = %v, want the client's exit code 3", err)
	}
	if *spawned != 0 || fake.logins != 0 {
		t.Errorf("spawned %d, logins %d; want neither", *spawned, fake.logins)
	}
	if ran.name != "claude" || !slices.Equal(ran.args, []string{"--resume", "-p", "hi"}) ||
		!slices.Contains(ran.env, "ANTHROPIC_BASE_URL=http://127.0.0.1:20999") {
		t.Errorf("ran %s %v", ran.name, ran.args)
	}
	if ran.attached != 1 {
		t.Errorf("attached clients while running = %d, want 1", ran.attached)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.attached.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.attached.Load() != 0 {
		t.Error("launcher stayed attached after the client exited")
	}
}

func TestLaunchStartsServerAndSignsInDefaultAccount(t *testing.T) {
	t.Parallel()
	fake := &fakeManagement{t: t}
	launcher, stderr, ran, spawned := newTestLauncher(t, fake)
	_ = launcher.launch(context.Background(), "codex", []string{"--yolo"})
	if *spawned != 1 || fake.logins != 1 {
		t.Fatalf("spawned %d, logins %d; want 1 each\n%s", *spawned, fake.logins, stderr)
	}
	for _, want := range []string{"started in the background", "Sign in in your browser", `saved Codex account "default"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if ran.name != "codex" || ran.args[len(ran.args)-1] != "--yolo" ||
		!slices.Contains(ran.args, `model_providers.rute.base_url="http://127.0.0.1:20999/v1"`) ||
		!slices.Contains(ran.args, `model_provider="rute"`) || !slices.Contains(ran.env, "RUTE_DOWNSTREAM_KEY=rute") {
		t.Errorf("ran %s %v", ran.name, ran.args)
	}
}

func TestLaunchReportsServerThatExits(t *testing.T) {
	t.Parallel()
	fake := &fakeManagement{t: t}
	launcher, _, _, _ := newTestLauncher(t, fake)
	launcher.spawn = func(string, string) (<-chan error, string, error) {
		exited := make(chan error, 1)
		exited <- io.ErrUnexpectedEOF
		return exited, "/state/rute.log", nil
	}
	err := launcher.launch(context.Background(), "claude", nil)
	if err == nil || !strings.Contains(err.Error(), "/state/rute.log") {
		t.Errorf("launch error = %v, want the log path", err)
	}
}

func TestAdminStartsServerAndOpensDashboard(t *testing.T) {
	t.Parallel()
	fake := &fakeManagement{t: t}
	launcher, _, _, spawned := newTestLauncher(t, fake)
	var opened string
	launcher.openBrowser = func(url string) error { opened = url; return nil }
	var stdout strings.Builder
	if err := launcher.openAdmin(context.Background(), &stdout); err != nil {
		t.Fatal(err)
	}
	if *spawned != 1 || !strings.HasPrefix(opened, "http://127.0.0.1:") || strings.TrimSpace(stdout.String()) != opened {
		t.Errorf("spawned %d, opened %q, printed %q", *spawned, opened, stdout.String())
	}
}
