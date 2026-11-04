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
	defer connection.CloseNow()
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
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	if records[0].Outcome != latency.OutcomeRateLimited || records[0].Attempts != 2 ||
		records[0].Endpoint != "responses" || records[1].Endpoint != "responses_ws" ||
		records[1].Outcome != latency.OutcomeRateLimited {
		t.Errorf("records = %+v", records)
	}
}
