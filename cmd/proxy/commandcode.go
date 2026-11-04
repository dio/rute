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
	"path/filepath"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/admin"
)

const (
	// commandCodeProviderID is the key written into ~/.commandcode/providers.json.
	commandCodeProviderID = "rute"
	// commandCodeCLIName is the Command Code CLI binary.
	commandCodeCLIName = "cmd"
)

// runLaunchCommandCode implements "rute cmd". Every argument goes to the
// Command Code CLI; a leading "--" is dropped.
func runLaunchCommandCode(
	ctx context.Context,
	args []string,
	stderr io.Writer,
	getenv func(string) string,
) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	return newLauncher(stderr, getenv).launchCommandCode(ctx, args)
}

// launchCommandCode signs in when rute has no accounts yet, registers rute as
// a BYOK provider (when absent), and runs the Command Code CLI.
func (launcher *launcher) launchCommandCode(ctx context.Context, args []string) error {
	if _, err := launcher.lookPath(commandCodeCLIName); err != nil {
		return fmt.Errorf("%s is not installed or not on PATH", commandCodeCLIName)
	}
	status, err := launcher.ensureServer(ctx)
	if err != nil {
		return err
	}
	proxyURL := status.ProxyURL
	if proxyURL == "" {
		proxyURL = "http://" + defaultListenAddress
	}
	if totalAccounts(status) == 0 {
		_, _ = fmt.Fprintln(launcher.stderr, "rute: no accounts yet; signing in to Command Code first")
		if err := launcher.ensureAccount(ctx, accountstore.ProviderCommandCode, status); err != nil {
			return err
		}
	}
	if err := ensureCommandCodeProvider(proxyURL, launcher.stderr); err != nil {
		_, _ = fmt.Fprintf(
			launcher.stderr,
			"rute: could not register the %q Command Code provider: %v\n",
			commandCodeProviderID,
			err,
		)
	}
	detach := launcher.attach()
	defer detach()
	return launcher.run(ctx, commandCodeCLIName, args, os.Environ())
}

// totalAccounts counts the accounts across every provider a running rute
// serves.
func totalAccounts(status admin.StatusResponse) int {
	total := 0
	for _, provider := range status.Providers {
		total += len(provider.Accounts)
	}
	return total
}

// commandCodeProvidersFile resolves the providers.json path. It is a variable
// so tests can redirect it.
var commandCodeProvidersFile = commandCodeProvidersPath

// ensureCommandCodeProvider registers the rute BYOK provider in
// ~/.commandcode/providers.json when it is missing.
func ensureCommandCodeProvider(proxyURL string, stderr io.Writer) error {
	path, err := commandCodeProvidersFile()
	if err != nil {
		return err
	}
	return ensureCommandCodeProviderAt(path, proxyURL, stderr)
}

// ensureCommandCodeProviderAt registers the rute BYOK provider in the given
// providers.json when it is missing. An existing entry is left untouched, and
// every other provider is preserved.
func ensureCommandCodeProviderAt(path, proxyURL string, stderr io.Writer) error {
	document, err := readProvidersFile(path)
	if err != nil {
		return err
	}
	providers, _ := document["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	if _, exists := providers[commandCodeProviderID]; exists {
		return nil
	}
	models, err := fetchProxyModels(proxyURL)
	if err != nil {
		return fmt.Errorf("list models from rute: %w", err)
	}
	if len(models) == 0 {
		return errors.New("rute exposed no models to register with Command Code")
	}
	providers[commandCodeProviderID] = commandCodeProvider(proxyURL, models)
	document["provider"] = providers
	if err := writeProvidersFile(path, document); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(
		stderr,
		"rute: registered the %q provider in %s; choose a rute/<model> model in /model\n",
		commandCodeProviderID,
		path,
	)
	return nil
}

// commandCodeProvider builds the BYOK provider entry. Claude models use the
// Anthropic wire (rute's /v1/messages); everything else uses the OpenAI
// Responses wire (rute's /v1/responses).
func commandCodeProvider(proxyURL string, models []commandCodeModel) map[string]any {
	entries := make(map[string]any, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		wire := "openai-responses"
		entry := map[string]any{"api": wire}
		if isAnthropicModel(model) {
			entry["api"] = "anthropic-messages"
			// The Anthropic wire appends /v1/messages to the root.
			entry["baseURL"] = proxyURL
		}
		if model.DisplayName != "" {
			entry["name"] = model.DisplayName
		}
		entries[model.ID] = entry
	}
	return map[string]any{
		"name":    commandCodeProviderID,
		"baseURL": strings.TrimRight(proxyURL, "/") + "/v1",
		"apiKey":  false,
		"api":     "openai-responses",
		"models":  entries,
	}
}

func isAnthropicModel(model commandCodeModel) bool {
	if model.OwnedBy == accountstore.ProviderClaude {
		return true
	}
	if model.OwnedBy == accountstore.ProviderCommandCode {
		return strings.HasPrefix(model.ID, "claude")
	}
	return false
}

type commandCodeModel struct {
	ID          string `json:"id"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name"`
}

func fetchProxyModels(proxyURL string) ([]commandCodeModel, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		strings.TrimRight(proxyURL, "/")+"/v1/models",
		nil,
	)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rute management returned %s", response.Status)
	}
	var payload struct {
		Data []commandCodeModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func commandCodeProvidersPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".commandcode", "providers.json"), nil
}

func readProvidersFile(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if document == nil {
		document = map[string]any{}
	}
	return document, nil
}

func writeProvidersFile(path string, document map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
