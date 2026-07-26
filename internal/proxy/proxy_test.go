package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/oauthpkce"
)

type staticCredentials struct {
	mu           sync.Mutex
	token        codexauth.Token
	forceRefresh int
	rateLimited  int
}

func (credentials *staticCredentials) Acquire(
	context.Context,
	string,
	map[string]struct{},
) (codexpool.Lease, error) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	return codexpool.Lease{AccountID: "static", Token: credentials.token}, nil
}

func (credentials *staticCredentials) ForceRefresh(
	context.Context,
	string,
) (codexpool.Lease, error) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	credentials.forceRefresh++
	credentials.token.AccessToken = "refreshed-token"
	return codexpool.Lease{AccountID: "static", Token: credentials.token}, nil
}

func (credentials *staticCredentials) MarkRateLimited(string, time.Time) {
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	credentials.rateLimited++
}

func (credentials *staticCredentials) MarkAuthorizationFailure(string) {
}

func TestResponsesHTTPNormalizesCodexRequest(t *testing.T) {
	t.Parallel()

	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("ChatGPT-Account-ID"); got != "account-1" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		writeTestSSE(writer)
	}))
	defer upstream.Close()

	handler := newTestHandler(t, upstream.URL, &staticCredentials{
		token: codexauth.Token{
			AccessToken: "access-token",
			AccountID:   "account-1",
		},
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{
			"model":"gpt-test",
			"input":[{"role":"system","content":"hello"}],
			"stream":false,
			"store":true,
			"max_output_tokens":12
		}`),
	)
	request.Header.Set("Authorization", "Bearer rute-local")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["id"] != "resp_test" {
		t.Errorf("response id = %v", response["id"])
	}
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Errorf("output item count = %d", len(output))
	}
	if upstreamBody["stream"] != true || upstreamBody["store"] != false {
		t.Errorf("stream/store = %v/%v", upstreamBody["stream"], upstreamBody["store"])
	}
	if _, exists := upstreamBody["max_output_tokens"]; exists {
		t.Error("max_output_tokens was forwarded")
	}
	input, _ := upstreamBody["input"].([]any)
	message, _ := input[0].(map[string]any)
	if message["role"] != "developer" {
		t.Errorf("input role = %v", message["role"])
	}
}

func TestRateLimitFailsOverAndRebindsSession(t *testing.T) {
	t.Parallel()

	var (
		mu             sync.Mutex
		authorizations []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization := request.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-first" {
			writer.Header().Set("Retry-After", "60")
			writeJSON(writer, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{
					"message": "usage limit reached",
					"code":    "usage_limit_reached",
				},
			})
			return
		}
		writeTestSSE(writer)
	}))
	defer upstream.Close()

	credentials := newTwoAccountPool(t)
	handler := newTestHandler(t, upstream.URL, credentials)
	for range 2 {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{
				"model":"gpt-test",
				"messages":[{"role":"user","content":"ping"}]
			}`),
		)
		request.Header.Set("X-Session-ID", "openworker-session")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"Bearer access-first",
		"Bearer access-second",
		"Bearer access-second",
	}
	if strings.Join(authorizations, ",") != strings.Join(want, ",") {
		t.Errorf("authorizations = %v, want %v", authorizations, want)
	}
}

func TestServerErrorIsNotReplayedAcrossAccounts(t *testing.T) {
	t.Parallel()

	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": "upstream failed"},
		})
	}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream.URL, newTwoAccountPool(t))
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"input":"ping"}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if requests != 1 {
		t.Errorf("upstream requests = %d, want 1", requests)
	}
}

func TestEmptyPoolReturnsAuthenticationRequired(t *testing.T) {
	t.Parallel()

	credentials, err := codexpool.New(
		context.Background(),
		accountstore.NewMemoryStore(),
		codexauth.Config{
			ClientID: "client",
			TokenURL: "https://example.invalid/token",
		},
		nil,
	)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	handler := newTestHandler(t, "http://127.0.0.1:1", credentials)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"input":"ping"}`),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"authentication_required"`) {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

func TestChatCompletionsNonStreaming(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if body["instructions"] != "answer briefly" {
			t.Errorf("instructions = %v", body["instructions"])
		}
		writeTestSSE(writer)
	}))
	defer upstream.Close()

	handler := newTestHandler(t, upstream.URL, &staticCredentials{
		token: codexauth.Token{AccessToken: "access-token"},
	})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"gpt-test",
			"messages":[
				{"role":"system","content":"answer briefly"},
				{"role":"user","content":"ping"}
			]
		}`),
	)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if completion.Object != "chat.completion" {
		t.Errorf("object = %q", completion.Object)
	}
	if got := completion.Choices[0].Message.Content; got != "pong" {
		t.Errorf("content = %q", got)
	}
	if completion.Usage.TotalTokens != 5 {
		t.Errorf("total tokens = %d", completion.Usage.TotalTokens)
	}
}

func TestResponsesWebSocket(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeTestSSE(writer)
	}))
	defer upstream.Close()
	handler := newTestHandler(t, upstream.URL, &staticCredentials{
		token: codexauth.Token{AccessToken: "access-token"},
	})
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(
		ctx,
		"ws"+strings.TrimPrefix(proxyServer.URL, "http")+"/v1/responses",
		nil,
	)
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	defer connection.CloseNow()

	if err := connection.Write(ctx, websocket.MessageText, []byte(
		`{"type":"response.create","model":"gpt-test","input":"ping"}`,
	)); err != nil {
		t.Fatalf("write WebSocket event: %v", err)
	}

	var eventTypes []string
	for {
		_, message, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read WebSocket event: %v", err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(message, &event); err != nil {
			t.Fatalf("decode WebSocket event: %v", err)
		}
		eventTypes = append(eventTypes, event.Type)
		if event.Type == "response.completed" {
			break
		}
	}
	if got := strings.Join(eventTypes, ","); got !=
		"response.output_text.delta,response.output_item.done,response.completed" {
		t.Errorf("event types = %q", got)
	}
}

func TestUpstreamUnauthorizedForcesOneRefresh(t *testing.T) {
	t.Parallel()

	var requests int
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer refreshed-token" {
			writeJSON(writer, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "expired"},
			})
			return
		}
		writeTestSSE(writer)
	}))
	defer upstream.Close()
	credentials := &staticCredentials{
		token: codexauth.Token{
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
		},
	}
	handler := newTestHandler(t, upstream.URL, credentials)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"input":"ping"}`),
	)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if requests != 2 {
		t.Errorf("upstream requests = %d", requests)
	}
	credentials.mu.Lock()
	defer credentials.mu.Unlock()
	if credentials.forceRefresh != 1 {
		t.Errorf("forced refreshes = %d", credentials.forceRefresh)
	}
}

func TestModelsListsDefaultModel(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t, "http://127.0.0.1:1", &staticCredentials{
		token: codexauth.Token{AccessToken: "access-token"},
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(response.Data) != 2 ||
		response.Data[0].ID != "codex/gpt-test" ||
		response.Data[1].ID != "gpt-test" {
		t.Errorf("models = %+v", response.Data)
	}
}

func newTestHandler(
	t *testing.T,
	upstreamURL string,
	credentials credentialSource,
) *Handler {
	t.Helper()
	handler, err := New(Config{
		UpstreamURL:  upstreamURL,
		DefaultModel: "gpt-test",
	}, credentials)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func newTwoAccountPool(t *testing.T) *codexpool.Pool {
	t.Helper()
	pool, err := codexpool.New(
		context.Background(),
		accountstore.NewMemoryStore(),
		codexauth.Config{
			ClientID: "client",
			TokenURL: "https://example.invalid/token",
		},
		nil,
	)
	if err != nil {
		t.Fatalf("new credential pool: %v", err)
	}
	for _, account := range []struct {
		name   string
		access string
	}{
		{name: "first", access: "access-first"},
		{name: "second", access: "access-second"},
	} {
		if _, err := pool.Add(context.Background(), account.name, oauthpkce.TokenSet{
			AccessToken:  account.access,
			RefreshToken: "refresh-" + account.name,
		}); err != nil {
			t.Fatalf("add %s account: %v", account.name, err)
		}
	}
	return pool
}

func writeTestSSE(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(
		writer,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"+
			"data: {\"type\":\"response.output_item.done\",\"item\":"+
			"{\"type\":\"message\",\"role\":\"assistant\",\"content\":"+
			"[{\"type\":\"output_text\",\"text\":\"pong\"}]}}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":"+
			"{\"id\":\"resp_test\",\"object\":\"response\",\"output\":"+
			"[],"+
			"\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n",
	)
}
