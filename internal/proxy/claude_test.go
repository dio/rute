package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/modelcatalog"
)

func TestClaudeMessagesForwardsOAuthProtocol(t *testing.T) {
	t.Parallel()
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.RawQuery != "beta=true" {
			t.Errorf("query = %q", request.URL.RawQuery)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer claude-access" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("x-api-key = %q, want empty", got)
		}
		beta := request.Header.Get("Anthropic-Beta")
		if !strings.Contains(beta, "client-capability") ||
			!strings.Contains(beta, "oauth-2025-04-20") {
			t.Errorf("anthropic beta = %q", beta)
		}
		if got := request.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Errorf("anthropic version = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Request-ID", "request-1")
		_, _ = writer.Write([]byte(`{"id":"msg_1","type":"message"}`))
	}))
	defer upstream.Close()

	handler := newClaudeTestHandler(t, upstream.URL)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"future_field":{"x":1}}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer downstream")
	request.Header.Set("X-Api-Key", "downstream")
	request.Header.Set("Anthropic-Beta", "client-capability")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamBody["model"] != "claude-sonnet-5" || upstreamBody["future_field"] == nil {
		t.Errorf("upstream body = %+v", upstreamBody)
	}
	if got := recorder.Header().Get("Request-ID"); got != "request-1" {
		t.Errorf("request ID = %q", got)
	}
}

func TestClaudeChatCompletionTranslatesTools(t *testing.T) {
	t.Parallel()
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"msg_1",
		  "content":[
		    {"type":"text","text":"checking"},
		    {"type":"tool_use","id":"tool_1","name":"weather","input":{"city":"Jakarta"}}
		  ],
		  "stop_reason":"tool_use",
		  "usage":{"input_tokens":10,"output_tokens":4}
		}`))
	}))
	defer upstream.Close()
	handler := newClaudeTestHandler(t, upstream.URL)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
		  "model":"claude/claude-sonnet-5",
		  "messages":[
		    {"role":"system","content":"Be concise"},
		    {"role":"user","content":"Weather?"}
		  ],
		  "tools":[{
		    "type":"function",
		    "function":{"name":"weather","parameters":{"type":"object"}}
		  }],
		  "stream":false
		}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamBody["model"] != "claude-sonnet-5" ||
		upstreamBody["system"] != "Be concise" ||
		upstreamBody["max_tokens"] == nil {
		t.Errorf("upstream body = %+v", upstreamBody)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := response["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || response["model"] != "claude/claude-sonnet-5" {
		t.Errorf("response = %+v", response)
	}
}

func TestClaudeChatCompletionStreamsOpenAIEvents(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: message_start\n")
		_, _ = io.WriteString(writer, `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2}}}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_1","name":"weather","input":{}}}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Jakarta\"}"}}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()
	handler := newClaudeTestHandler(t, upstream.URL)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
		  "model":"claude/claude-sonnet-5",
		  "messages":[{"role":"user","content":"hi"}],
		  "stream":true,
		  "stream_options":{"include_usage":true}
		}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if count := bytes.Count(recorder.Body.Bytes(), []byte("data: [DONE]")); count != 1 {
		t.Fatalf("done count = %d, body = %s", count, recorder.Body.String())
	}
	scanner := bufio.NewScanner(strings.NewReader(recorder.Body.String()))
	foundText := false
	foundTool := false
	foundArguments := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"content":"hello"`) {
			foundText = true
		}
		if strings.Contains(scanner.Text(), `"name":"weather"`) {
			foundTool = true
		}
		if strings.Contains(scanner.Text(), `"arguments":"{\"city\":\"Jakarta\"}"`) {
			foundArguments = true
		}
	}
	if !foundText || !foundTool || !foundArguments {
		t.Errorf("stream = %s", recorder.Body.String())
	}
}

func TestModelMappingRoutesChatToClaude(t *testing.T) {
	t.Parallel()
	upstreamModel := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		upstreamModel <- body["model"].(string)
		_, _ = writer.Write([]byte(
			`{"id":"msg_1","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{}}`,
		))
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(
		`[{"id":"worker","provider":"claude","upstream_model":"claude-sonnet-5"}]`,
	), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	catalog, err := modelcatalog.Load(path)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	handler, err := NewMulti(
		Config{
			UpstreamURL:       "http://127.0.0.1:1",
			ClaudeUpstreamURL: upstream.URL,
			DefaultModel:      "gpt-test",
			ModelCatalog:      catalog,
		},
		&staticCredentials{token: codexauth.Token{AccessToken: "codex-access"}},
		&staticCredentials{token: codexauth.Token{AccessToken: "claude-access"}},
	)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(
			`{"model":"worker","messages":[{"role":"user","content":"hello"}]}`,
		),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := <-upstreamModel; got != "claude-sonnet-5" {
		t.Errorf("upstream model = %q", got)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["model"] != "worker" {
		t.Errorf("public model = %v", response["model"])
	}
}

func TestClaudeChatRejectsUnsupportedOpenAIFields(t *testing.T) {
	t.Parallel()
	_, _, err := openAIToClaudeChat(map[string]any{
		"model":           "claude-sonnet-5",
		"messages":        []any{map[string]any{"role": "user", "content": "hello"}},
		"response_format": map[string]any{"type": "json_object"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "response_format") {
		t.Fatalf("error = %v", err)
	}
}

func newClaudeTestHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	handler, err := NewMulti(
		Config{
			UpstreamURL:       "http://127.0.0.1:1",
			ClaudeUpstreamURL: upstreamURL + "?beta=true",
			DefaultModel:      "gpt-test",
		},
		&staticCredentials{
			token: codexauth.Token{AccessToken: "codex-access"},
		},
		&staticCredentials{
			token: codexauth.Token{AccessToken: "claude-access"},
		},
	)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}
