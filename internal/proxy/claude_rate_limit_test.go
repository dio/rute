package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dio/rute/internal/accountstore"
)

func TestClaudeRateLimitSurvivesFailoverAndCooldown(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "60")
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
					"type": "rate_limit_error", "message": "Error",
				}})
			}))
			defer upstream.Close()
			handler, err := NewMulti(Config{
				UpstreamURL: "http://127.0.0.1:1", ClaudeUpstreamURL: upstream.URL,
				DefaultModel: "gpt-test",
			}, map[string]CredentialSource{
				accountstore.ProviderCodex:  &staticCredentials{},
				accountstore.ProviderClaude: newTwoAccountPool(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(
					`{"model":"claude/claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusTooManyRequests {
					t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body)
				}
				retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
				if err != nil || retry < 1 || retry > 60 {
					t.Fatalf("retry-after = %q", rec.Header().Get("Retry-After"))
				}
				if !strings.Contains(rec.Body.String(), "rate_limit_error") {
					t.Fatalf("body = %s", rec.Body)
				}
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("upstream calls = %d, want 2 (none during cooldown)", got)
			}
		})
	}
}

func TestClaudeAuthorizationCooldownIsNotReportedAsRateLimit(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			pool := newTwoAccountPool(t)
			for _, account := range pool.Accounts() {
				pool.MarkAuthorizationFailure(account.ID)
			}
			handler, err := NewMulti(Config{UpstreamURL: "http://127.0.0.1:1", DefaultModel: "gpt-test"},
				map[string]CredentialSource{
					accountstore.ProviderCodex:  &staticCredentials{},
					accountstore.ProviderClaude: pool,
				})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(
				`{"model":"claude/claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "rate_limit_error") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestClaudeRequestRejectionDoesNotCoolDownAccounts(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"type": "error", "error": map[string]string{
			"type": "rate_limit_error", "message": "Error",
		}})
	}))
	defer upstream.Close()
	pool := newTwoAccountPool(t)
	handler, err := NewMulti(Config{
		UpstreamURL: "http://127.0.0.1:1", ClaudeUpstreamURL: upstream.URL,
		DefaultModel: "gpt-test",
	}, map[string]CredentialSource{
		accountstore.ProviderCodex:  &staticCredentials{},
		accountstore.ProviderClaude: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
			`{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"Error"`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want one per request with no failover or cooldown", got)
	}
	for account, count := range pool.InFlight() {
		if count != 0 {
			t.Errorf("account %s holds %d leases", account, count)
		}
	}
}
