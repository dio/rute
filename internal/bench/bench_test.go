package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/internal/proxy"
	"github.com/dio/rute/oauthpkce"
)

const (
	firstTokenDelay = 30 * time.Millisecond
	// tokenGap spaces four tokens past the minimum generation window that
	// latency.Record.Fill requires before reporting an output rate.
	tokenGap = 40 * time.Millisecond
)

type tokenCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (counter *tokenCounter) add(request *http.Request) {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.counts[request.Header.Get("Authorization")]++
}

func codexUpstream(counter *tokenCounter) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		counter.add(request)
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		_, _ = io.WriteString(writer, `data: {"type":"response.created","response":{}}`+"\n\n")
		flusher.Flush()
		time.Sleep(firstTokenDelay)
		for range 4 {
			_, _ = io.WriteString(writer, `data: {"type":"response.output_text.delta","delta":"tok "}`+"\n\n")
			flusher.Flush()
			time.Sleep(tokenGap)
		}
		_, _ = io.WriteString(writer, `data: {"type":"response.output_item.done","item":`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tok tok tok tok"}]}}`+"\n\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","output":[],`+
			`"usage":{"input_tokens":3,"output_tokens":4}}}`+"\n\n")
	}
}

func claudeUpstream(counter *tokenCounter) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		counter.add(request)
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		send := func(event, data string) {
			_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		send("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message",`+
			`"role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		time.Sleep(firstTokenDelay)
		for range 4 {
			send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tok "}}`)
			time.Sleep(tokenGap)
		}
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		send("message_stop", `{"type":"message_stop"}`)
	}
}

func newPool(t *testing.T, provider string) *codexpool.Pool {
	t.Helper()
	pool, err := codexpool.NewProvider(
		context.Background(),
		accountstore.NewMemoryStore(),
		provider,
		codexauth.Config{ClientID: "client", TokenURL: "https://example.invalid/token"},
		nil,
	)
	if err != nil {
		t.Fatalf("new %s pool: %v", provider, err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := pool.Add(context.Background(), name, oauthpkce.TokenSet{
			AccessToken:  provider + "-" + name,
			RefreshToken: "refresh-" + name,
		}); err != nil {
			t.Fatalf("add %s account: %v", name, err)
		}
	}
	return pool
}

func TestBenchThroughProxyBalancesAccounts(t *testing.T) {
	t.Parallel()

	codexCalls := &tokenCounter{counts: map[string]int{}}
	claudeCalls := &tokenCounter{counts: map[string]int{}}
	codexServer := httptest.NewServer(codexUpstream(codexCalls))
	defer codexServer.Close()
	claudeServer := httptest.NewServer(claudeUpstream(claudeCalls))
	defer claudeServer.Close()

	metricsPath := filepath.Join(t.TempDir(), "calls.jsonl")
	metrics, err := latency.OpenWriter(metricsPath)
	if err != nil {
		t.Fatalf("open metrics: %v", err)
	}
	codexPool := newPool(t, accountstore.ProviderCodex)
	claudePool := newPool(t, accountstore.ProviderClaude)
	handler, err := proxy.NewMulti(proxy.Config{
		UpstreamURL:       codexServer.URL,
		ClaudeUpstreamURL: claudeServer.URL,
		DefaultModel:      "gpt-test",
		Metrics:           metrics,
	}, codexPool, claudePool)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	runs := []struct {
		api   string
		model string
	}{
		{APIMessages, "claude-test"},
		{APIChat, "claude/claude-test"},
		{APIChat, "gpt-test"},
		{APIResponses, "gpt-test"},
		{APIResponsesWS, "gpt-test"},
	}
	for _, run := range runs {
		records, err := Run(context.Background(), Config{
			BaseURL:   server.URL + "/v1",
			API:       run.api,
			Model:     run.model,
			Levels:    []int{1, 4},
			Rounds:    2,
			Prompt:    "ping",
			MaxTokens: 16,
			Timeout:   10 * time.Second,
		})
		if err != nil {
			t.Fatalf("%s %s: %v", run.api, run.model, err)
		}
		if len(records) != 10 {
			t.Fatalf("%s %s: %d records, want 10", run.api, run.model, len(records))
		}
		for _, record := range records {
			if record.Outcome != latency.OutcomeOK {
				t.Fatalf("%s %s: outcome %q", run.api, run.model, record.Outcome)
			}
			if record.OutputTokens != 4 || record.WaitMS < float64(firstTokenDelay.Milliseconds()) ||
				record.TokensPerSecond <= 0 {
				t.Errorf("%s %s: record %+v", run.api, run.model, record)
			}
			if record.InFlight < 1 || record.InFlight > record.Concurrency {
				t.Errorf("%s %s: in flight %d at concurrency %d",
					run.api, run.model, record.InFlight, record.Concurrency)
			}
		}
	}
	if err := metrics.Close(); err != nil {
		t.Fatalf("close metrics: %v", err)
	}

	for name, pool := range map[string]*codexpool.Pool{"codex": codexPool, "claude": claudePool} {
		for account, count := range pool.InFlight() {
			if count != 0 {
				t.Errorf("%s account %s has %d leases after the run", name, account, count)
			}
		}
	}
	for name, counter := range map[string]*tokenCounter{"codex": codexCalls, "claude": claudeCalls} {
		if len(counter.counts) != 2 {
			t.Fatalf("%s calls by token = %v, want both accounts", name, counter.counts)
		}
		low, total := 0, 0
		for _, count := range counter.counts {
			total += count
			if low == 0 || count < low {
				low = count
			}
		}
		// Sessions are sticky, so the split is uneven by a session or two;
		// neither account should take the bulk.
		if low*4 < total {
			t.Errorf("%s calls are unbalanced: %v", name, counter.counts)
		}
	}

	file, err := os.Open(metricsPath)
	if err != nil {
		t.Fatalf("open metrics: %v", err)
	}
	defer func() { _ = file.Close() }()
	records, err := latency.ReadRecords(file)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if len(records) != 50 {
		t.Fatalf("proxy recorded %d calls, want 50", len(records))
	}
	endpoints := map[string]int{}
	for _, record := range records {
		endpoints[record.Provider+"/"+record.Endpoint]++
		if record.Outcome != latency.OutcomeOK || record.Account == "" ||
			record.InFlight < 1 || record.OutputTokens != 4 || record.WaitMS <= 0 {
			t.Errorf("proxy record %+v", record)
		}
	}
	want := map[string]int{
		"claude/messages":         10,
		"claude/chat_completions": 10,
		"codex/chat_completions":  10,
		"codex/responses":         10,
		"codex/responses_ws":      10,
	}
	for key, count := range want {
		if endpoints[key] != count {
			t.Errorf("endpoints = %v", endpoints)
			break
		}
	}
}
