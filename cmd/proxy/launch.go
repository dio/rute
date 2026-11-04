package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/admin"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/serverfile"
)

const (
	defaultAdminURL   = "http://127.0.0.1:20129"
	launchIdleTimeout = 3 * time.Hour
	startTimeout      = 20 * time.Second
	// raceGrace is how long to look for another launcher's server after
	// the one this launcher started exits.
	raceGrace = 2 * time.Second
)

// exitCodeError ends rute with a client's exit code and no message.
type exitCodeError int

func (code exitCodeError) Error() string {
	return fmt.Sprintf("exit status %d", int(code))
}

// launcher starts or reuses a background rute and runs a client through it.
type launcher struct {
	// candidates returns the management URLs to try, in order.
	candidates func() []string
	// adminURL is the management URL of the server in use.
	adminURL string
	stderr   io.Writer
	client   *http.Client
	// spawn starts rute serve in the background and returns a channel that
	// receives its exit error.
	spawn       func() (<-chan error, string, error)
	openBrowser func(url string) error
	lookPath    func(name string) (string, error)
	run         func(ctx context.Context, name string, args, env []string) error
	poll        time.Duration
}

func newLauncher(stderr io.Writer, getenv func(string) string) *launcher {
	return &launcher{
		candidates:  func() []string { return adminCandidates(getenv) },
		stderr:      stderr,
		client:      &http.Client{Timeout: 5 * time.Second},
		spawn:       spawnServe,
		openBrowser: openBrowser,
		lookPath:    exec.LookPath,
		run:         runClient,
		poll:        200 * time.Millisecond,
	}
}

// runLaunch implements "rute claude" and "rute codex". Every argument goes to
// the client; a leading "--" is dropped.
func runLaunch(ctx context.Context, provider string, args []string, stderr io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	return newLauncher(stderr, getenv).launch(ctx, provider, args)
}

func (launcher *launcher) launch(ctx context.Context, provider string, args []string) error {
	name, err := clientName(provider)
	if err != nil {
		return err
	}
	if _, err := launcher.lookPath(name); err != nil {
		return fmt.Errorf("%s is not installed or not on PATH", name)
	}
	status, err := launcher.ensureServer(ctx)
	if err != nil {
		return err
	}
	if err := launcher.ensureAccount(ctx, provider, status); err != nil {
		return err
	}
	detach := launcher.attach()
	defer detach()

	proxyURL := status.ProxyURL
	if proxyURL == "" {
		proxyURL = "http://" + defaultListenAddress
	}
	return launcher.run(ctx, name, clientArgs(provider, proxyURL, args), clientEnv(provider, proxyURL))
}

func clientName(provider string) (string, error) {
	switch provider {
	case accountstore.ProviderClaude, accountstore.ProviderCodex:
		return provider, nil
	}
	return "", fmt.Errorf("unknown client %q", provider)
}

// clientArgs returns the client's arguments. Codex reads the rute provider
// from config overrides, which leaves the user's config.toml untouched.
func clientArgs(provider, proxyURL string, args []string) []string {
	if provider != accountstore.ProviderCodex {
		return args
	}
	overrides := []string{
		"-c", `model_provider="rute"`,
		"-c", `model_providers.rute.name="rute"`,
		"-c", `model_providers.rute.base_url="` + proxyURL + `/v1"`,
		"-c", `model_providers.rute.env_key="RUTE_DOWNSTREAM_KEY"`,
		"-c", `model_providers.rute.wire_api="responses"`,
		"-c", `model_providers.rute.supports_websockets=true`,
	}
	return append(overrides, args...)
}

func clientEnv(provider, proxyURL string) []string {
	env := os.Environ()
	switch provider {
	case accountstore.ProviderClaude:
		env = append(env, "ANTHROPIC_BASE_URL="+proxyURL)
	case accountstore.ProviderCodex:
		// Codex requires a provider key; rute ignores it.
		env = append(env, "RUTE_DOWNSTREAM_KEY=rute")
	}
	return env
}

func (launcher *launcher) status(ctx context.Context) (admin.StatusResponse, error) {
	var status admin.StatusResponse
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, launcher.adminURL+"/v1/status", nil)
	if err != nil {
		return status, err
	}
	response, err := launcher.client.Do(request)
	if err != nil {
		return status, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("rute management returned %s", response.Status)
	}
	return status, json.NewDecoder(response.Body).Decode(&status)
}

// adminCandidates returns RUTE_ADMIN_URL when set; otherwise the URL in the
// state directory's server.json, then the default management address.
func adminCandidates(getenv func(string) string) []string {
	if url := strings.TrimRight(getenv("RUTE_ADMIN_URL"), "/"); url != "" {
		return []string{url}
	}
	var urls []string
	if dir, err := accountstore.DefaultStateDir(); err == nil {
		if info, err := serverfile.Read(filepath.Join(dir, "server.json")); err == nil && info.AdminURL != "" {
			urls = append(urls, info.AdminURL)
		}
	}
	return append(urls, defaultAdminURL)
}

// discover returns the status of the first candidate server that answers and
// uses it for later requests.
func (launcher *launcher) discover(ctx context.Context) (admin.StatusResponse, error) {
	err := errors.New("no rute server found")
	for _, url := range launcher.candidates() {
		launcher.adminURL = url
		var status admin.StatusResponse
		if status, err = launcher.status(ctx); err == nil {
			return status, nil
		}
	}
	return admin.StatusResponse{}, err
}

// ensureServer returns the status of a running rute, starting one in the
// background when none answers.
func (launcher *launcher) ensureServer(ctx context.Context) (admin.StatusResponse, error) {
	if status, err := launcher.discover(ctx); err == nil {
		return status, nil
	}
	exited, logPath, err := launcher.spawn()
	if err != nil {
		return admin.StatusResponse{}, fmt.Errorf("start rute: %w", err)
	}
	// A server that exits early may have lost a start race to another
	// launcher, so keep looking until the deadline.
	var exitErr error
	deadline := time.Now().Add(startTimeout)
	for {
		if status, err := launcher.discover(ctx); err == nil {
			if exitErr == nil {
				_, _ = fmt.Fprintf(launcher.stderr,
					"rute: started in the background; it stops %s after the last client exits (log: %s)\n",
					launchIdleTimeout, logPath)
			}
			return status, nil
		}
		select {
		case exitErr = <-exited:
			if exitErr == nil {
				exitErr = errors.New("exited")
			}
			exited = nil
			if grace := time.Now().Add(raceGrace); grace.Before(deadline) {
				deadline = grace
			}
		case <-ctx.Done():
			return admin.StatusResponse{}, ctx.Err()
		case <-time.After(launcher.poll):
		}
		if time.Now().After(deadline) {
			if exitErr != nil {
				return admin.StatusResponse{}, fmt.Errorf("rute serve exited (%v); see %s", exitErr, logPath)
			}
			return admin.StatusResponse{}, fmt.Errorf("rute did not start within %s; see %s", startTimeout, logPath)
		}
	}
}

// ensureAccount signs in a "default" account when provider has none.
func (launcher *launcher) ensureAccount(ctx context.Context, provider string, status admin.StatusResponse) error {
	var pool *codexpool.Status
	for index := range status.Providers {
		if status.Providers[index].Provider == provider {
			pool = &status.Providers[index]
		}
	}
	if pool == nil {
		return fmt.Errorf("the running rute does not serve %s accounts", provider)
	}
	if len(pool.Accounts) > 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"provider": provider, "name": "default", "action": admin.ActionAdd})
	var login admin.Login
	if err := launcher.postJSON(ctx, "/v1/logins", body, &login); err != nil {
		return fmt.Errorf("start %s login: %w", provider, err)
	}
	_, _ = fmt.Fprintf(launcher.stderr, "rute: no %s account yet. Sign in in your browser:\n  %s\n",
		provider, login.AuthorizationURL)
	if err := launcher.openBrowser(login.AuthorizationURL); err != nil {
		_, _ = fmt.Fprintln(launcher.stderr, "rute: could not open a browser; open the link above")
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(launcher.poll * 5):
		}
		if err := launcher.getJSON(ctx, "/v1/logins/"+login.ID, &login); err != nil {
			return fmt.Errorf("check %s login: %w", provider, err)
		}
		switch login.State {
		case admin.LoginDone:
			_, _ = fmt.Fprintf(launcher.stderr, "rute: saved %s account \"default\"\n", provider)
			return nil
		case admin.LoginFailed:
			return fmt.Errorf("%s login failed: %s", provider, login.Error)
		}
	}
}

func (launcher *launcher) postJSON(ctx context.Context, path string, body []byte, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, launcher.adminURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return launcher.do(request, target)
}

func (launcher *launcher) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, launcher.adminURL+path, nil)
	if err != nil {
		return err
	}
	return launcher.do(request, target)
}

func (launcher *launcher) do(request *http.Request, target any) error {
	response, err := launcher.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&failure)
		if failure.Error.Message != "" {
			return errors.New(failure.Error.Message)
		}
		return fmt.Errorf("rute management returned %s", response.Status)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

// attach holds a request open so the background rute stays up while the
// client runs, and returns the function that releases it.
func (launcher *launcher) attach() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, launcher.adminURL+"/v1/attach", nil)
		if err != nil {
			return
		}
		response, err := (&http.Client{}).Do(request)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	return func() {
		cancel()
		<-done
	}
}

// spawnServe starts "rute serve" detached from the terminal, logging to the
// state directory.
func spawnServe() (<-chan error, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	dir, err := accountstore.DefaultStateDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	logPath := filepath.Join(dir, "rute.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = logFile.Close() }()
	command := exec.Command(executable, "serve",
		"-listen=127.0.0.1:0", "-admin-listen=127.0.0.1:0",
		"-idle-timeout="+launchIdleTimeout.String(), "-initial-login=false")
	command.Stdout = logFile
	command.Stderr = logFile
	command.Dir = dir
	// A new session keeps rute running when the terminal closes.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, "", err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	return exited, logPath, nil
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// runClient runs the client in the foreground and returns its exit code. The
// terminal delivers Ctrl-C to the client directly, so rute waits for it to
// exit instead of stopping it.
func runClient(_ context.Context, name string, args, env []string) error {
	command := exec.Command(name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = env
	err := command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitCodeError(exitErr.ExitCode())
	}
	return err
}

// runAdmin opens the management dashboard in a browser, starting rute in the
// background when none is running.
func runAdmin(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) != 0 {
		return errors.New("admin does not accept arguments")
	}
	return newLauncher(stderr, getenv).openAdmin(ctx, stdout)
}

func (launcher *launcher) openAdmin(ctx context.Context, stdout io.Writer) error {
	if _, err := launcher.ensureServer(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, launcher.adminURL)
	if err := launcher.openBrowser(launcher.adminURL); err != nil {
		_, _ = fmt.Fprintln(launcher.stderr, "rute: could not open a browser; open the link above")
	}
	return nil
}
