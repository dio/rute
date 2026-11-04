package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/oauthpkce"
)

type fakeSession struct {
	tokens chan oauthpkce.TokenSet
}

func (session *fakeSession) AuthorizationURL() string {
	return "https://claude.ai/oauth/authorize?state=test"
}

func (session *fakeSession) Wait(ctx context.Context) (oauthpkce.TokenSet, error) {
	select {
	case tokens := <-session.tokens:
		return tokens, nil
	case <-ctx.Done():
		return oauthpkce.TokenSet{}, ctx.Err()
	}
}

func (session *fakeSession) Close() error { return nil }

type fixture struct {
	server  *Server
	pool    *codexpool.Pool
	calls   *callstore.Store
	session *fakeSession
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool, err := codexpool.NewProvider(context.Background(), accountstore.NewMemoryStore(),
		accountstore.ProviderClaude,
		codexauth.Config{ClientID: "client", TokenURL: "https://example.invalid/token"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"default", "personal"} {
		if _, err := pool.Add(context.Background(), name, oauthpkce.TokenSet{
			AccessToken: "secret-access-" + name, RefreshToken: "secret-refresh-" + name,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.SetBalance(codexpool.BalancePriority, []string{"default", "personal"}); err != nil {
		t.Fatal(err)
	}
	pool.SetMaxInFlight(6)
	calls, err := callstore.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = calls.Close() })
	session := &fakeSession{tokens: make(chan oauthpkce.TokenSet, 1)}
	server, err := New(Config{
		Version:      "test",
		Pools:        map[string]*codexpool.Pool{accountstore.ProviderClaude: pool},
		Calls:        calls,
		StartSession: func(string) (Session, error) { return session, nil },
		LoginTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{server: server, pool: pool, calls: calls, session: session}
}

func (fixture *fixture) do(t *testing.T, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	fixture.server.ServeHTTP(recorder, request)
	return recorder
}

func TestRejectsNonLoopbackHost(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	for _, host := range []string{"evil.example:20129", "192.168.1.5:20129", "rute.tailnet.ts.net"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		request.Host = host
		if got := fixture.do(t, request).Code; got != http.StatusForbidden {
			t.Errorf("host %s: status = %d, want 403", host, got)
		}
	}
	for _, host := range []string{"127.0.0.1:20129", "localhost:20129", "[::1]:20129"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		request.Host = host
		if got := fixture.do(t, request).Code; got != http.StatusOK {
			t.Errorf("host %s: status = %d, want 200", host, got)
		}
	}
}

func TestStatusShowsAccountsWithoutCredentials(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	lease, err := fixture.pool.Acquire(context.Background(), "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.pool.Release(lease.AccountID)
	fixture.pool.MarkRateLimited(lease.AccountID, time.Now().Add(time.Hour), "seven_day")

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/status", nil)
	recorder := fixture.do(t, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "secret-") {
		t.Fatalf("status leaks credentials: %s", recorder.Body)
	}
	var status StatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	provider := status.Providers[0]
	if provider.Balance != codexpool.BalancePriority || provider.MaxInFlight != 6 ||
		strings.Join(provider.Order, ",") != "default,personal" {
		t.Errorf("provider = %+v", provider)
	}
	first := provider.Accounts[0]
	if first.Name != "default" || first.InFlight != 1 || !first.RateLimited ||
		first.Reason != "seven_day" || first.CooldownUntil == nil {
		t.Errorf("default account = %+v", first)
	}
}

func TestLoginAPIAddsAccountToRunningPool(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)

	form := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader("provider=claude&name=third"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := fixture.do(t, form).Code; got != http.StatusUnsupportedMediaType {
		t.Fatalf("form-encoded login status = %d, want 415", got)
	}

	start := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
			strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return fixture.do(t, request)
	}
	if got := start(`{"provider":"claude","name":"default"}`).Code; got != http.StatusBadRequest {
		t.Errorf("duplicate add status = %d, want 400", got)
	}
	recorder := start(`{"provider":"claude","name":"third"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("start login = %d: %s", recorder.Code, recorder.Body)
	}
	var login Login
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.State != LoginPending || !strings.HasPrefix(login.AuthorizationURL, "https://claude.ai/") {
		t.Fatalf("login = %+v", login)
	}
	if got := start(`{"provider":"claude","name":"fourth"}`).Code; got != http.StatusConflict {
		t.Errorf("second login status = %d, want 409", got)
	}

	fixture.session.tokens <- oauthpkce.TokenSet{AccessToken: "secret-a", RefreshToken: "secret-r"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/logins/"+login.ID, nil)
		var polled Login
		if err := json.Unmarshal(fixture.do(t, request).Body.Bytes(), &polled); err != nil {
			t.Fatal(err)
		}
		login = polled
		if login.State != LoginPending || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if login.State != LoginDone || login.AuthorizationURL != "" {
		t.Fatalf("finished login = %+v", login)
	}
	var names []string
	for _, account := range fixture.pool.Accounts() {
		names = append(names, account.Name)
	}
	if strings.Join(names, ",") != "default,personal,third" {
		t.Errorf("accounts = %v", names)
	}
}

func TestDashboardFormsRequireCSRF(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	post := func(values url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/accounts",
			strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return fixture.do(t, request)
	}
	values := url.Values{"provider": {"claude"}, "name": {"third"}, "action": {"add"}}
	if got := post(values).Code; got != http.StatusForbidden {
		t.Fatalf("form without token = %d, want 403", got)
	}

	page := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/accounts/new", nil))
	match := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page.Body.String())
	if match == nil {
		t.Fatalf("no CSRF token in form:\n%s", page.Body)
	}
	values.Set("csrf", match[1])
	recorder := post(values)
	if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/logins/") {
		t.Fatalf("form with token = %d, location %q", recorder.Code, recorder.Header().Get("Location"))
	}
	loginPage := fixture.do(t, httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:20129"+recorder.Header().Get("Location"), nil))
	if !strings.Contains(loginPage.Body.String(), "https://claude.ai/oauth/authorize?state=test") ||
		!strings.Contains(loginPage.Body.String(), `http-equiv="refresh"`) {
		t.Errorf("login page:\n%s", loginPage.Body)
	}
}

func TestDashboardShowsAccountsAndHistory(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	now := time.Now().UTC()
	for _, record := range []latency.Record{
		{Time: now.Add(-time.Minute), Source: "proxy", Provider: "claude", Endpoint: "messages",
			Account: "default", InFlight: 1, Status: 429, Outcome: latency.OutcomeRateLimited, DurationMS: 300},
		{Time: now, Source: "proxy", Provider: "claude", Endpoint: "messages", Account: "personal",
			InFlight: 2, Status: 200, Outcome: latency.OutcomeOK, WaitMS: 2100, DurationMS: 5000,
			OutputTokens: 400, TokensPerSecond: 82},
	} {
		if err := fixture.calls.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	recorder := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/?window=15m", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"claude: 2 accounts (2 active)",
		"priority (default &gt; personal), max 6 in flight",
		"claude/personal", "2-3", "2.1s", "82",
		"rate_limited",
		`href="/accounts/new?provider=claude"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "secret-") {
		t.Error("dashboard leaks credentials")
	}

	summary := fixture.do(t, httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:20129/v1/calls/summary?window=15m", nil))
	var response struct {
		Rows []SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(summary.Body.Bytes(), &response); err != nil {
		t.Fatalf("summary JSON: %v\n%s", err, summary.Body)
	}
	if len(response.Rows) != 2 || response.Rows[0].WaitP50MS != nil || *response.Rows[1].WaitP50MS != 2100 {
		t.Errorf("summary rows = %+v", response.Rows)
	}
}
