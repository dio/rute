package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/rute/internal/codexauth"
)

func TestWebSocketContinuationKeepsToolContext(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		input, _ := body["input"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(input) == 1 && input[0].(map[string]any)["type"] == "function_call_output" {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"missing_tool_call\",\"message\":\"No matching tool call\"}}\n\n")
			return
		}
		if len(input) > 1 {
			if len(input) != 3 || input[1].(map[string]any)["call_id"] != "call_1" {
				t.Errorf("lost transcript: %#v", input)
			}
			if _, exists := body["previous_response_id"]; exists {
				t.Error("forwarded previous_response_id")
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}]}}\n\n")
	}))
	defer upstream.Close()
	server := httptest.NewServer(newTestHandler(t, upstream.URL, &staticCredentials{token: codexauth.Token{AccessToken: "test"}}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	for _, request := range []string{
		`{"type":"response.create","model":"gpt-test","input":"Look this up"}`,
		`{"type":"response.create","model":"gpt-test","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"found"}]}`,
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != "response.completed" {
			t.Fatalf("continuation failed: %s", data)
		}
	}

	// A new connection must not inherit the first connection's transcript.
	other, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.CloseNow() }()
	if err := other.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-test","previous_response_id":"resp_1","input":"continue"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := other.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"code":"previous_response_not_found"`) {
		t.Fatalf("connection reused another transcript: %s", data)
	}
	// The connection remains usable for a full-context retry.
	if err := other.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-test","input":"fresh"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err = other.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"response.completed"`) {
		t.Fatalf("full-context retry failed: %s", data)
	}

}

func TestContinuationIsolationAndRecovery(t *testing.T) {
	t.Parallel()
	const limit = 4096
	var state responseContinuation
	turn := responseTurn{input: []any{messageItem("user", "input_text", "first")}, finished: true}
	turn.observe([]byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`))
	state.remember(&turn, "gpt-test", limit)
	original := string(state.input)
	for _, tc := range []struct {
		name, id, model string
		empty           bool
		limit           int64
		want            string
	}{
		{name: "unknown", id: "resp_unknown", model: "gpt-test", limit: limit, want: "previous_response_not_found"},
		{name: "other connection", id: "resp_first", model: "gpt-test", empty: true, limit: limit, want: "previous_response_not_found"},
		{name: "other model", id: "resp_first", model: "other", limit: limit, want: "previous_response_not_found"},
		{name: "expanded size limit", id: "resp_first", model: "gpt-test", limit: 10, want: "invalid_request_error"},
		{name: "continuation", id: "resp_first", model: "gpt-test", limit: limit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := state
			if tc.empty {
				current = responseContinuation{}
			}
			body := map[string]any{"previous_response_id": tc.id, "input": "next"}
			code, err := current.expand(body, tc.model, tc.limit)
			if code != tc.want || (err != nil) != (tc.want != "") {
				t.Fatalf("code=%q err=%v", code, err)
			}
			if err == nil {
				input := body["input"].([]any)
				if len(input) != 4 || input[1].(map[string]any)["encrypted_content"] != "opaque" {
					t.Fatalf("input=%#v", input)
				}
				input[0].(map[string]any)["role"] = "changed"
				if string(state.input) != original {
					t.Fatal("normalization changed saved transcript")
				}
			}
		})
	}
	for _, tc := range []struct {
		name, event string
		finished    bool
	}{
		{"failed", `{"type":"response.failed","response":{"id":"resp_bad","error":{"message":"failed"}}}`, true},
		{"interrupted", `{"type":"response.completed","response":{"id":"resp_bad","output":[]}}`, false},
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_bad","output":[]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := responseTurn{finished: tc.finished}
			failed.observe([]byte(tc.event))
			state.remember(&failed, "gpt-test", limit)
			if state.id != "resp_first" {
				t.Fatalf("lost valid state after %s", tc.name)
			}
		})
	}
	replacement := responseTurn{input: []any{messageItem("user", "input_text", "fresh")}, finished: true}
	replacement.observe([]byte(`{"type":"response.completed","response":{"id":"resp_new","output":[]}}`))
	state.remember(&replacement, "gpt-test", limit)
	body := map[string]any{"previous_response_id": "resp_new", "input": []any{}}
	if _, err := state.expand(body, "gpt-test", limit); err != nil {
		t.Fatal(err)
	}
	if len(body["input"].([]any)) != 1 {
		t.Fatal("full input resent with stale history")
	}
	state.remember(&replacement, "gpt-test", 1)
	if state.id != "" || state.input != nil {
		t.Fatal("oversized transcript retained")
	}
}
