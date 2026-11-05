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
	"sort"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/admin"
	"github.com/dio/rute/providers/commandcode"
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

// launchCommandCode signs in when Command Code has no account, registers rute
// as a BYOK provider, and runs the Command Code CLI.
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
	if count, served := providerAccountCount(status, accountstore.ProviderCommandCode); served && count == 0 {
		imported, importErr := launcher.importCommandCodeKey(ctx)
		if importErr != nil {
			_, _ = fmt.Fprintf(launcher.stderr, "rute: could not import the Command Code key: %v\n", importErr)
		}
		if !imported {
			if totalAccounts(status) == 0 {
				_, _ = fmt.Fprintln(launcher.stderr, "rute: no accounts yet; signing in to Command Code first")
				if err := launcher.ensureAccount(ctx, accountstore.ProviderCommandCode, status); err != nil {
					return err
				}
			} else {
				_, _ = fmt.Fprintln(launcher.stderr,
					"rute: no Command Code account; run 'rute account add -provider=commandcode' to add one")
			}
		}
	}
	// Re-read status so a just-added account counts as available.
	if refreshed, err := launcher.status(ctx); err == nil {
		status = refreshed
	}
	home, err := launcher.clientHome(commandCodeCLIName)
	if err != nil {
		return err
	}
	providersPath := filepath.Join(home, ".commandcode", "providers.json")
	models := map[string]any{}
	if key, ok := launcher.commandCodeKey(); ok {
		catalog, err := fetchCommandCodeModels(key)
		if err != nil {
			_, _ = fmt.Fprintf(launcher.stderr, "rute: could not list Command Code models: %v\n", err)
		} else {
			for id, entry := range catalogModelEntries(catalog, proxyURL) {
				models[id] = entry
			}
		}
	}
	if ruteModels, err := fetchProxyModels(proxyURL); err != nil {
		_, _ = fmt.Fprintf(launcher.stderr, "rute: could not list rute models: %v\n", err)
	} else {
		available := map[string]bool{}
		for _, provider := range status.Providers {
			available[provider.Provider] = len(provider.Accounts) > 0
		}
		for id, entry := range ruteModelEntries(ruteModels, proxyURL, available) {
			models[id] = entry
		}
	}
	if err := registerCommandCodeProviderAt(providersPath, proxyURL, models, launcher.stderr); err != nil {
		_, _ = fmt.Fprintf(
			launcher.stderr,
			"rute: could not register the %q Command Code provider: %v\n",
			commandCodeProviderID,
			err,
		)
	}
	detach := launcher.attach()
	defer detach()
	return launcher.run(
		ctx,
		commandCodeCLIName,
		commandCodeArgs(args, pickCommandCodeModel(models)),
		commandCodeEnv(home),
	)
}

// commandCodeEnv gives cmd rute's prepared home, so its own ~/.commandcode
// login, providers, and plan are not used.
func commandCodeEnv(home string) []string {
	return append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
}

// commandCodeArgs keeps every request on rute: --local-only disables the
// Command Code backend, and the session starts on a rute model unless the user
// chose one.
func commandCodeArgs(args []string, model string) []string {
	hasModel := false
	hasLocalOnly := false
	for _, arg := range args {
		switch {
		case arg == "-m", arg == "--model",
			strings.HasPrefix(arg, "--model="), strings.HasPrefix(arg, "-m="):
			hasModel = true
		case arg == "--local-only":
			hasLocalOnly = true
		}
	}
	result := make([]string, 0, len(args)+3)
	if !hasLocalOnly {
		result = append(result, "--local-only")
	}
	if !hasModel && model != "" {
		result = append(result, "-m", model)
	}
	return append(result, args...)
}

// pickCommandCodeModel chooses the rute model a session starts on: the model
// cmd was already using when rute offers it, otherwise a Command Code model,
// otherwise any registered model.
func pickCommandCodeModel(models map[string]any) string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	if configured := commandCodeConfiguredModel(); configured != "" {
		want := accountstore.ProviderCommandCode + "/" + configured
		if _, ok := models[want]; ok {
			return commandCodeProviderID + "/" + want
		}
	}
	for _, id := range ids {
		if strings.HasPrefix(id, accountstore.ProviderCommandCode+"/") {
			return commandCodeProviderID + "/" + id
		}
	}
	return commandCodeProviderID + "/" + ids[0]
}

// commandCodeConfigFile resolves cmd's config file; a variable so tests can
// redirect it.
var commandCodeConfigFile = commandCodeConfigPath

func commandCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".commandcode", "config.json"), nil
}

// commandCodeConfiguredModel returns the model cmd is configured to use.
func commandCodeConfiguredModel() string {
	path, err := commandCodeConfigFile()
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var config struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &config) != nil {
		return ""
	}
	return strings.TrimSpace(config.Model)
}

// providerAccountCount returns a provider's account count and whether the
// running rute serves that provider.
func providerAccountCount(status admin.StatusResponse, provider string) (int, bool) {
	for _, item := range status.Providers {
		if item.Provider == provider {
			return len(item.Accounts), true
		}
	}
	return 0, false
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

// commandCodeAuthFile resolves cmd's own auth file. It is a variable so tests
// can redirect it.
var commandCodeAuthFile = commandCodeAuthPath

func commandCodeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".commandcode", "auth.json"), nil
}

// commandCodeAuthKey is the part of cmd's auth.json rute reads.
type commandCodeAuthKey struct {
	APIKey string `json:"apiKey"`
}

// commandCodeKey returns the key cmd itself signed in with, when present.
func (launcher *launcher) commandCodeKey() (string, bool) {
	path, err := commandCodeAuthFile()
	if err != nil {
		return "", false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var auth commandCodeAuthKey
	if json.Unmarshal(body, &auth) != nil {
		return "", false
	}
	apiKey := strings.TrimSpace(auth.APIKey)
	return apiKey, apiKey != ""
}

// importCommandCodeKey stores the key cmd already signed in with as a rute
// account, so an authenticated CLI does not need a second browser login.
func (launcher *launcher) importCommandCodeKey(ctx context.Context) (bool, error) {
	apiKey, ok := launcher.commandCodeKey()
	if !ok {
		return false, nil
	}
	path, err := commandCodeAuthFile()
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]string{
		"provider": accountstore.ProviderCommandCode,
		"name":     "default",
		"action":   admin.ActionAdd,
		"api_key":  apiKey,
	})
	if err != nil {
		return false, err
	}
	var login admin.Login
	if err := launcher.postJSON(ctx, "/v1/logins", payload, &login); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintf(
		launcher.stderr,
		"rute: imported the Command Code key from %s as account \"default\"\n",
		path,
	)
	return true, nil
}

// ensureCommandCodeProviderAt registers the models rute reports.
func ensureCommandCodeProviderAt(path, proxyURL string, stderr io.Writer) error {
	models, err := fetchProxyModels(proxyURL)
	if err != nil {
		return fmt.Errorf("list models from rute: %w", err)
	}
	if len(models) == 0 {
		return errors.New("rute exposed no models to register with Command Code")
	}
	entries := ruteModelEntries(models, proxyURL, nil)
	return registerCommandCodeProviderAt(path, proxyURL, entries, stderr)
}

// registerCommandCodeProviderAt points the rute entry at the running proxy and
// registers models. Every other provider is preserved, and models already on
// the rute entry are kept.
//
// The background rute picks free ports, so its proxy URL changes between runs;
// the entry must be rewritten each time.
func registerCommandCodeProviderAt(
	path, proxyURL string,
	models map[string]any,
	stderr io.Writer,
) error {
	if len(models) == 0 {
		return errors.New("no models to register with Command Code")
	}
	document, err := readProvidersFile(path)
	if err != nil {
		return err
	}
	providers, _ := document["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	before, err := json.Marshal(document)
	if err != nil {
		return err
	}
	providers[commandCodeProviderID] = commandCodeProviderEntry(
		providers[commandCodeProviderID],
		proxyURL,
		models,
	)
	document["provider"] = providers
	after, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
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

// commandCodeProviderEntry updates the rute BYOK entry, keeping a user's extra
// models and other fields.
func commandCodeProviderEntry(existing any, proxyURL string, models map[string]any) map[string]any {
	entry, _ := existing.(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["name"] = commandCodeProviderID
	entry["baseURL"] = strings.TrimRight(proxyURL, "/") + "/v1"
	entry["apiKey"] = false
	if _, ok := entry["api"]; !ok {
		entry["api"] = "openai-responses"
	}
	merged := map[string]any{}
	if prior, ok := entry["models"].(map[string]any); ok {
		for id, value := range prior {
			merged[id] = value
		}
	}
	for id, value := range models {
		merged[id] = value
	}
	entry["models"] = merged
	return entry
}

// ruteModelEntries builds entries for the models rute reports. A non-nil
// available set keeps only models whose provider has an account, so a provider
// with none is not offered.
func ruteModelEntries(
	models []commandCodeModel,
	proxyURL string,
	available map[string]bool,
) map[string]any {
	entries := make(map[string]any, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if available != nil && !available[model.OwnedBy] {
			continue
		}
		entry := map[string]any{"api": "openai-responses"}
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
	return entries
}

// catalogModelEntries builds entries for Command Code's own models, routed
// through rute under the commandcode/ namespace.
func catalogModelEntries(models []commandCodeModel, proxyURL string) map[string]any {
	entries := make(map[string]any, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		wire := model.wire()
		entry := map[string]any{"api": wire}
		if wire == "anthropic-messages" {
			// The Anthropic wire appends /v1/messages to the root.
			entry["baseURL"] = proxyURL
		}
		switch {
		case model.Name != "":
			entry["name"] = model.Name
		case model.DisplayName != "":
			entry["name"] = model.DisplayName
		}
		if model.ContextLength > 0 {
			entry["contextWindow"] = model.ContextLength
		}
		entries[accountstore.ProviderCommandCode+"/"+model.ID] = entry
	}
	return entries
}

// wire picks the wire for a Command Code model from the endpoints it serves.
func (model commandCodeModel) wire() string {
	for _, endpoint := range model.SupportedEndpoints {
		if endpoint == "/responses" {
			return "openai-responses"
		}
	}
	for _, endpoint := range model.SupportedEndpoints {
		switch endpoint {
		case "/chat/completions":
			return "openai-completions"
		case "/messages":
			return "anthropic-messages"
		}
	}
	return "openai-responses"
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
	ID                 string   `json:"id"`
	OwnedBy            string   `json:"owned_by"`
	DisplayName        string   `json:"display_name"`
	Name               string   `json:"name"`
	ContextLength      int      `json:"context_length"`
	SupportedEndpoints []string `json:"supported_endpoints"`
}

// fetchCommandCodeModels lists the models the account's plan can call.
func fetchCommandCodeModels(apiKey string) ([]commandCodeModel, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		commandCodeAPIBaseURL+commandcode.ModelsPath,
		nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Command Code returned %s", response.Status)
	}
	var payload struct {
		Data []commandCodeModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

// commandCodeAPIBaseURL is the Provider API root; a variable so tests can
// redirect it.
var commandCodeAPIBaseURL = commandcode.APIBaseURL

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
