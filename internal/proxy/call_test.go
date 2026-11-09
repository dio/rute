package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/rute/internal/latency"
)

func TestCodexRateLimitIsReportedAndRecorded(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
			"type": "usage_limit_reached", "message": "limit",
		}})
	}))
	defer upstream.Close()

	metricsPath := filepath.Join(t.TempDir(), "calls.jsonl")
	metrics, err := latency.OpenWriter(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := newTwoAccountPool(t)
	handler, err := New(Config{
		UpstreamURL:  upstream.URL,
		DefaultModel: "gpt-test",
		Metrics:      metrics,
	}, pool)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"ping","stream":true}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" ||
		!strings.Contains(recorder.Body.String(), "rate_limit_error") {
		t.Fatalf("status = %d, headers = %v, body = %s", recorder.Code, recorder.Header(), recorder.Body)
	}

	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = connection.CloseNow() }()
	if err := connection.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-test","input":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.Read(ctx)
	if err != nil || !strings.Contains(string(message), "rate_limit_error") {
		t.Fatalf("WebSocket message = %s, err = %v", message, err)
	}

	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want one per account", got)
	}
	for account, count := range pool.InFlight() {
		if count != 0 {
			t.Errorf("account %s holds %d leases", account, count)
		}
	}
	if err := metrics.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	records, err := latency.ReadRecords(file)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first call reached accounts; the WebSocket call found every
	// account rate-limited, so it has no account to record.
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	if records[0].Outcome != latency.OutcomeRateLimited || records[0].Attempts != 2 ||
		records[0].Endpoint != "responses" || records[0].Account == "" {
		t.Errorf("record = %+v", records[0])
	}
}

func TestStreamErrorDetailsAreRecorded(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"missing_tool_call\",\"message\":\"No matching tool call\"}}}\n\n"))
	}))
	defer upstream.Close()
	sink := &recordSink{}
	handler, err := New(Config{UpstreamURL: upstream.URL, DefaultModel: "gpt-test", Metrics: sink}, newTwoAccountPool(t))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"ping","stream":true}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.records) != 1 {
		t.Fatalf("records=%+v", sink.records)
	}
	record := sink.records[0]
	if record.Status != 200 || record.Outcome != latency.OutcomeUpstreamError || record.ErrorCode != "missing_tool_call" || record.ErrorMessage != "No matching tool call" || record.OutputTokens != 0 {
		t.Fatalf("record=%+v", record)
	}
}
