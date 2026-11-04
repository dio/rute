package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
)

type capturedRequest struct {
	path          string
	authorization string
	apiKey        string
	model         string
}

func TestCommandCodeRoutesAndInjectsAPIKey(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		captured []capturedRequest
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		model, _ := body["model"].(string)
		mu.Lock()
		captured = append(captured, capturedRequest{
			path:          request.URL.Path,
			authorization: request.Header.Get("Authorization"),
			apiKey:        request.Header.Get("X-Api-Key"),
			model:         model,
		})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/messages":
			_, _ = writer.Write([]byte(`{"id":"msg_cc","type":"message","content":[]}`))
		case "/responses":
			_, _ = writer.Write([]byte(`{"id":"resp_cc","object":"response","output":[]}`))
		default:
			_, _ = writer.Write([]byte(`{"id":"chatcmpl-cc","object":"chat.completion"}`))
		}
	}))
	defer upstream.Close()

	handler, err := NewMulti(Config{
		UpstreamURL:            "http://127.0.0.1:1",
		ClaudeUpstreamURL:      "http://127.0.0.1:1",
		CommandCodeUpstreamURL: upstream.URL,
		DefaultModel:           "gpt-test",
	}, map[string]CredentialSource{
		accountstore.ProviderCodex: &staticCredentials{
			token: codexauth.Token{AccessToken: "codex-access"},
		},
		accountstore.ProviderCommandCode: &staticCredentials{
			token: codexauth.Token{AccessToken: "cc-key"},
		},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	requests := []struct {
		path  string
		body  string
		model string
	}{
		{
			path:  "/v1/messages",
			body:  `{"model":"commandcode/claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			model: "claude-sonnet-4-6",
		},
		{
			path:  "/v1/responses",
			body:  `{"model":"commandcode/deepseek/deepseek-v4-flash","input":"hi"}`,
			model: "deepseek/deepseek-v4-flash",
		},
		{
			path:  "/v1/chat/completions",
			body:  `{"model":"commandcode/deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`,
			model: "deepseek/deepseek-v4-flash",
		},
	}
	for _, test := range requests {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer downstream")
		request.Header.Set("X-Api-Key", "downstream")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", test.path, recorder.Code, recorder.Body.String())
		}
	}
	if len(captured) != len(requests) {
		t.Fatalf("captured %d upstream requests, want %d", len(captured), len(requests))
	}
	for index, test := range requests {
		got := captured[index]
		if got.authorization != "Bearer cc-key" {
			t.Errorf("%s authorization = %q", test.path, got.authorization)
		}
		if got.apiKey != "" {
			t.Errorf("%s x-api-key = %q, want empty", test.path, got.apiKey)
		}
		if got.model != test.model {
			t.Errorf("%s upstream model = %q, want %q", test.path, got.model, test.model)
		}
	}
	if captured[0].path != "/messages" ||
		captured[1].path != "/responses" ||
		captured[2].path != "/chat/completions" {
		t.Errorf("paths = %q", []string{captured[0].path, captured[1].path, captured[2].path})
	}
}
