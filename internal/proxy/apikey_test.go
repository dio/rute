package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
)

func TestCodexAPIKeyUsesOpenAIEndpoint(t *testing.T) {
	t.Parallel()
	var captured struct {
		path          string
		authorization string
		originator    string
		accountID     string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured.path = request.URL.Path
		captured.authorization = request.Header.Get("Authorization")
		captured.originator = request.Header.Get("originator")
		captured.accountID = request.Header.Get("ChatGPT-Account-ID")
		writeTestSSE(writer)
	}))
	defer upstream.Close()

	handler, err := New(Config{
		UpstreamURL:  "http://127.0.0.1:1",
		CodexAPIURL:  upstream.URL,
		DefaultModel: "gpt-test",
	}, &staticCredentials{
		token:    codexauth.Token{AccessToken: "sk-openai", AccountID: "acct-1"},
		authMode: accountstore.AuthModeAPIKey,
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"ping","stream":false}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if captured.authorization != "Bearer sk-openai" {
		t.Errorf("authorization = %q", captured.authorization)
	}
	if captured.originator != "" || captured.accountID != "" {
		t.Errorf("Codex CLI headers leaked to the API: %+v", captured)
	}
	if captured.path != "/" {
		t.Errorf("path = %q, want the API root", captured.path)
	}
}

func TestClaudeAPIKeyUsesAnthropicEndpoint(t *testing.T) {
	t.Parallel()
	var captured struct {
		apiKey     string
		authHeader string
		beta       string
		version    string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured.apiKey = request.Header.Get("X-Api-Key")
		captured.authHeader = request.Header.Get("Authorization")
		captured.beta = request.Header.Get("Anthropic-Beta")
		captured.version = request.Header.Get("Anthropic-Version")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_1","type":"message","content":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewMulti(Config{
		UpstreamURL:       "http://127.0.0.1:1",
		ClaudeUpstreamURL: "http://127.0.0.1:1",
		ClaudeAPIURL:      upstream.URL,
		DefaultModel:      "gpt-test",
	}, map[string]CredentialSource{
		accountstore.ProviderCodex: &staticCredentials{
			token: codexauth.Token{AccessToken: "codex-access"},
		},
		accountstore.ProviderClaude: &staticCredentials{
			token:    codexauth.Token{AccessToken: "sk-ant-key"},
			authMode: accountstore.AuthModeAPIKey,
		},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
	)
	request.Header.Set("Authorization", "Bearer downstream")
	request.Header.Set("X-Api-Key", "downstream")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if captured.apiKey != "sk-ant-key" {
		t.Errorf("x-api-key = %q", captured.apiKey)
	}
	if captured.authHeader != "" {
		t.Errorf("authorization = %q, want empty", captured.authHeader)
	}
	if captured.version != "2023-06-01" {
		t.Errorf("anthropic-version = %q", captured.version)
	}
	if strings.Contains(captured.beta, "oauth-2025-04-20") {
		t.Errorf("API-key request carried the OAuth capability: %q", captured.beta)
	}
}

func TestClaudeAPIKeyTranslatesChatCompletions(t *testing.T) {
	t.Parallel()
	var model string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		model, _ = body["model"].(string)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer upstream.Close()

	handler, err := NewMulti(Config{
		UpstreamURL:       "http://127.0.0.1:1",
		ClaudeUpstreamURL: "http://127.0.0.1:1",
		ClaudeAPIURL:      upstream.URL,
		DefaultModel:      "gpt-test",
	}, map[string]CredentialSource{
		accountstore.ProviderCodex: &staticCredentials{
			token: codexauth.Token{AccessToken: "codex-access"},
		},
		accountstore.ProviderClaude: &staticCredentials{
			token:    codexauth.Token{AccessToken: "sk-ant-key"},
			authMode: accountstore.AuthModeAPIKey,
		},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"claude/claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if model != "claude-sonnet-5" {
		t.Errorf("upstream model = %q", model)
	}
}
