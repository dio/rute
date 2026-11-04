package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/internal/pricing"
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
		StartSession: func(string) (Pending, error) { return session, nil },
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
	if !strings.Contains(body, `<meta http-equiv="refresh" content="5">`) {
		t.Error("dashboard does not refresh every 5 seconds by default")
	}
	for query, want := range map[string]string{"refresh=1m": `content="60"`, "refresh=off": ""} {
		page := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/?"+query, nil)).Body.String()
		hasRefresh := strings.Contains(page, `http-equiv="refresh"`)
		if want == "" && hasRefresh || want != "" && !strings.Contains(page, want) {
			t.Errorf("%s: refresh tag present = %v", query, hasRefresh)
		}
		if !strings.Contains(page, query+`" class="on"`) {
			t.Errorf("%s: current refresh choice is not marked", query)
		}
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

func TestRoutingAPIChangesRunningPool(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	put := func(contentType, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:20129/v1/providers/claude/routing",
			strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		return fixture.do(t, request)
	}
	if got := put("text/plain", `{"balance":"spread"}`).Code; got != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON body = %d, want 415", got)
	}
	for _, body := range []string{
		`{"balance":"spread","order":["personal"]}`,
		`{"balance":"priority","max_in_flight":0}`,
		`{"balance":"failover","order":["missing"]}`,
		`{"balance":"sideways"}`,
	} {
		if got := put("application/json", body).Code; got != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, got)
		}
	}
	if status := fixture.pool.Status(); status.Balance != codexpool.BalancePriority || status.MaxInFlight != 6 {
		t.Fatalf("rejected change altered the pool: %+v", status)
	}

	recorder := put("application/json", `{"balance":"failover","order":["personal","default"],"max_in_flight":3}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid change = %d: %s", recorder.Code, recorder.Body)
	}
	status := fixture.pool.Status()
	if status.Balance != codexpool.BalanceFailover || status.MaxInFlight != 3 ||
		strings.Join(status.Order, ",") != "personal,default" {
		t.Errorf("status after change = %+v", status)
	}

	request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:20129/v1/providers/gemini/routing",
		strings.NewReader(`{"balance":"spread"}`))
	request.Header.Set("Content-Type", "application/json")
	if got := fixture.do(t, request).Code; got != http.StatusNotFound {
		t.Errorf("unknown provider = %d, want 404", got)
	}
}

func TestRoutingFormChangesRunningPool(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	page := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/providers/claude/routing", nil))
	body := page.Body.String()
	if !strings.Contains(body, `<option value="priority" selected>`) || !strings.Contains(body, `value="default, personal"`) {
		t.Fatalf("routing page does not show current routing:\n%s", body)
	}
	match := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if match == nil {
		t.Fatal("no CSRF token in routing form")
	}
	post := func(values url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/providers/claude/routing",
			strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return fixture.do(t, request)
	}
	values := url.Values{"balance": {"spread"}, "order": {""}, "max_in_flight": {"0"}}
	if got := post(values).Code; got != http.StatusForbidden {
		t.Errorf("form without token = %d, want 403", got)
	}
	values.Set("csrf", match[1])
	values.Set("order", "personal")
	if recorder := post(values); recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "requires the priority or failover") {
		t.Errorf("invalid form = %d:\n%s", recorder.Code, recorder.Body)
	}
	values.Set("order", "")
	if recorder := post(values); recorder.Code != http.StatusSeeOther {
		t.Fatalf("valid form = %d:\n%s", recorder.Code, recorder.Body)
	}
	if status := fixture.pool.Status(); status.Balance != codexpool.BalanceSpread || status.MaxInFlight != 0 || len(status.Order) != 0 {
		t.Errorf("status after form = %+v", status)
	}
}

func TestUsageReportsTokensAndEstimatedCost(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	prices := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(prices, []byte(`{"models":{"claude-test-*":{"input":2,"output":10,"cache_read":0.5,"cache_write":4}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.server.config.Prices = pricing.NewFile(prices)
	now := time.Now()
	for _, record := range []latency.Record{
		{Time: now.Add(-2 * time.Hour), Provider: "claude", Account: "default", Model: "claude-test-1",
			Outcome: latency.OutcomeOK, InputTokens: 1_000_000, OutputTokens: 100_000,
			CacheReadTokens: 2_000_000, CacheWriteTokens: 250_000},
		{Time: now, Provider: "claude", Account: "personal", Model: "unlisted", Outcome: latency.OutcomeOK,
			InputTokens: 10, OutputTokens: 20},
	} {
		_ = fixture.calls.Write(record)
	}

	recorder := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/usage?window=24h&by=total", nil))
	var response struct {
		Rows     []UsageRow `json:"rows"`
		Unpriced []string   `json:"unpriced_models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("usage JSON: %v\n%s", err, recorder.Body)
	}
	if len(response.Rows) != 2 || response.Rows[0].CostUSD == nil || *response.Rows[0].CostUSD != 2+1+1+1 ||
		response.Rows[1].CostUSD != nil || strings.Join(response.Unpriced, ",") != "unlisted" {
		t.Fatalf("usage = %s", recorder.Body)
	}
	if response.Rows[0].Start != nil {
		t.Error("total rows have a start time")
	}

	hourly := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/usage?window=24h", nil))
	if err := json.Unmarshal(hourly.Body.Bytes(), &response); err != nil || len(response.Rows) != 2 || response.Rows[0].Start == nil {
		t.Errorf("hourly usage = %s", hourly.Body)
	}
	if got := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/usage?window=1y", nil)).Code; got != http.StatusBadRequest {
		t.Errorf("bad window = %d, want 400", got)
	}

	page := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/usage?window=24h", nil)).Body.String()
	for _, want := range []string{"claude/default", "claude-test-1", "1.00M", "2.00M", "250.0k", "$5.00", "No price for: unlisted", "By hour"} {
		if !strings.Contains(page, want) {
			t.Errorf("usage page missing %q", want)
		}
	}
}

func TestRestartFromDashboardAndAPI(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	page := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/", nil)).Body.String()
	if strings.Contains(page, `action="/restart"`) {
		t.Error("restart button shown without a restart function")
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/restart", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	if got := fixture.do(t, request).Code; got != http.StatusServiceUnavailable {
		t.Errorf("restart without a function = %d, want 503", got)
	}

	var restarts atomic.Int32
	fixture.server.config.Restart = func() error { restarts.Add(1); return nil }
	page = fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/", nil)).Body.String()
	match := regexp.MustCompile(`action="/restart" class="inline"><input type="hidden" name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("no restart button:\n%s", page)
	}
	post := func(values url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/restart", strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return fixture.do(t, request)
	}
	if got := post(url.Values{}).Code; got != http.StatusForbidden || restarts.Load() != 0 {
		t.Errorf("form without token = %d, restarts %d", got, restarts.Load())
	}
	if recorder := post(url.Values{"csrf": {match[1]}}); recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), `content="3; url=/"`) || restarts.Load() != 1 {
		t.Errorf("form restart = %d, restarts %d", recorder.Code, restarts.Load())
	}
	for contentType, want := range map[string]int{"text/plain": http.StatusUnsupportedMediaType, "application/json": http.StatusAccepted} {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/restart", strings.NewReader("{}"))
		request.Header.Set("Content-Type", contentType)
		if got := fixture.do(t, request).Code; got != want {
			t.Errorf("API restart with %s = %d, want %d", contentType, got, want)
		}
	}
	if restarts.Load() != 2 {
		t.Errorf("restarts = %d, want 2", restarts.Load())
	}
}
