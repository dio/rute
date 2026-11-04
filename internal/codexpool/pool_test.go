package codexpool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/oauthpkce"
)

func TestPoolAuditsAccountLifecycle(t *testing.T) {
	t.Parallel()

	store, err := accountstore.NewFileStore(
		filepath.Join(t.TempDir(), "state", "credentials.json"),
	)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: "https://example.invalid/token",
	}, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	added, err := pool.Add(context.Background(), "personal", oauthpkce.TokenSet{
		AccessToken:  "first-access",
		RefreshToken: "first-refresh",
	})
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := pool.Reauth(context.Background(), added.Name, oauthpkce.TokenSet{
		AccessToken:  "second-access",
		RefreshToken: "second-refresh",
	}); err != nil {
		t.Fatalf("reauthenticate account: %v", err)
	}
	if err := pool.Delete(context.Background(), added.Name); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	events, err := store.AuditHistory(context.Background(), accountstore.AuditFilter{})
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	want := []accountstore.AuditAction{
		accountstore.AuditAccountAdded,
		accountstore.AuditAccountReauthenticated,
		accountstore.AuditAccountRemoved,
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(events), len(want))
	}
	for index, action := range want {
		if events[index].Action != action {
			t.Errorf("event %d action = %q, want %q", index, events[index].Action, action)
		}
		if events[index].RecordID != added.ID {
			t.Errorf("event %d record ID = %q, want %q", index, events[index].RecordID, added.ID)
		}
	}
}

func TestPoolKeepsCredentialMutationWhenAuditAppendFails(t *testing.T) {
	t.Parallel()

	store, err := accountstore.NewFileStore(
		filepath.Join(t.TempDir(), "state", "credentials.json"),
	)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	var logs bytes.Buffer
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: "https://example.invalid/token",
	}, &logs)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if _, err := pool.Add(context.Background(), "personal", oauthpkce.TokenSet{
		AccessToken:  "first-access",
		RefreshToken: "first-refresh",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	body, err := os.ReadFile(store.AuditPath())
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	body = []byte(strings.Replace(
		string(body),
		string(accountstore.AuditAccountAdded),
		string(accountstore.AuditAccountRemoved),
		1,
	))
	if err := os.WriteFile(store.AuditPath(), body, 0o600); err != nil {
		t.Fatalf("corrupt audit history: %v", err)
	}
	if err := pool.Reauth(context.Background(), "personal", oauthpkce.TokenSet{
		AccessToken:  "second-access",
		RefreshToken: "second-refresh",
	}); err != nil {
		t.Fatalf("reauthenticate with corrupt audit history: %v", err)
	}
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].RefreshToken != "second-refresh" {
		t.Errorf("stored accounts = %+v", accounts)
	}
	if !strings.Contains(logs.String(), "credential change succeeded") {
		t.Errorf("logs = %q, want audit coverage warning", logs.String())
	}
}

func TestPoolBootstrapPersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode refresh request: %v", err)
			}
			if body["refresh_token"] != "old-refresh" {
				t.Errorf("refresh token = %q", body["refresh_token"])
			}
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"expires_in":3600
			}`)
		},
	))
	defer tokenServer.Close()

	store, err := accountstore.NewFileStore(
		filepath.Join(t.TempDir(), "state", "credentials.json"),
	)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	record := accountstore.Account{
		ID:           "profile-1",
		Name:         "personal",
		AccountID:    "upstream-1",
		RefreshToken: "old-refresh",
		State:        accountstore.StateActive,
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: tokenServer.URL,
	}, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := pool.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	records, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list store: %v", err)
	}
	if got := records[0].RefreshToken; got != "new-refresh" {
		t.Errorf("stored refresh token = %q", got)
	}
	lease, err := pool.Acquire(context.Background(), "session-1", nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease.Token.AccessToken != "new-access" ||
		lease.Token.AccountID != "upstream-1" {
		t.Errorf("lease token = %v", lease.Token)
	}
	events, err := store.AuditHistory(context.Background(), accountstore.AuditFilter{})
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	if len(events) != 1 || events[0].Action != accountstore.AuditCredentialsRefreshed {
		t.Errorf("audit events = %+v", events)
	}
}

func TestPoolSessionStickyRoundRobinAndCooldown(t *testing.T) {
	t.Parallel()

	store := accountstore.NewMemoryStore()
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: "https://example.invalid/token",
	}, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	first, err := pool.Add(context.Background(), "first", oauthpkce.TokenSet{
		AccessToken:  "access-first",
		RefreshToken: "refresh-first",
	})
	if err != nil {
		t.Fatalf("add first account: %v", err)
	}
	second, err := pool.Add(context.Background(), "second", oauthpkce.TokenSet{
		AccessToken:  "access-second",
		RefreshToken: "refresh-second",
	})
	if err != nil {
		t.Fatalf("add second account: %v", err)
	}

	firstLease, err := pool.Acquire(context.Background(), "session-a", nil)
	if err != nil {
		t.Fatalf("acquire first session: %v", err)
	}
	stickyLease, err := pool.Acquire(context.Background(), "session-a", nil)
	if err != nil {
		t.Fatalf("reacquire first session: %v", err)
	}
	secondLease, err := pool.Acquire(context.Background(), "session-b", nil)
	if err != nil {
		t.Fatalf("acquire second session: %v", err)
	}
	if firstLease.AccountID != first.ID || stickyLease.AccountID != first.ID {
		t.Errorf("first session leases = %q, %q; want %q", firstLease.AccountID, stickyLease.AccountID, first.ID)
	}
	if secondLease.AccountID != second.ID {
		t.Errorf("second session lease = %q, want %q", secondLease.AccountID, second.ID)
	}

	pool.MarkRateLimited(first.ID, time.Now().Add(time.Minute))
	failoverLease, err := pool.Acquire(context.Background(), "session-a", nil)
	if err != nil {
		t.Fatalf("acquire after cooldown: %v", err)
	}
	if failoverLease.AccountID != second.ID {
		t.Errorf("failover lease = %q, want %q", failoverLease.AccountID, second.ID)
	}
}

func TestPoolPermanentRefreshFailureMarksReauthentication(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(writer, `{"error":"invalid_grant"}`)
		},
	))
	defer tokenServer.Close()

	store, err := accountstore.NewFileStore(
		filepath.Join(t.TempDir(), "state", "credentials.json"),
	)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	if err := store.Put(context.Background(), accountstore.Account{
		ID:           "profile-1",
		Name:         "expired",
		RefreshToken: "invalid-refresh",
		State:        accountstore.StateActive,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: tokenServer.URL,
	}, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := pool.Bootstrap(context.Background()); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("bootstrap error = %v, want authentication required", err)
	}
	accounts := pool.Accounts()
	if got := accounts[0].State; got != accountstore.StateReauthRequired {
		t.Errorf("account state = %q", got)
	}
	if _, err := pool.Acquire(context.Background(), "session", nil); !errors.Is(err, ErrAuthenticationRequired) {
		t.Errorf("acquire error = %v, want authentication required", err)
	}
	events, err := store.AuditHistory(context.Background(), accountstore.AuditFilter{})
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	if len(events) != 1 || events[0].Action != accountstore.AuditReauthRequired {
		t.Errorf("audit events = %+v", events)
	}
}

type flakyStore struct {
	inner    *accountstore.MemoryStore
	mu       sync.Mutex
	failPuts int
}

func (store *flakyStore) List(ctx context.Context) ([]accountstore.Account, error) {
	return store.inner.List(ctx)
}

func (store *flakyStore) Put(ctx context.Context, account accountstore.Account) error {
	store.mu.Lock()
	if store.failPuts > 0 {
		store.failPuts--
		store.mu.Unlock()
		return errors.New("injected persistence failure")
	}
	store.mu.Unlock()
	return store.inner.Put(ctx, account)
}

func (store *flakyStore) Delete(ctx context.Context, id string) error {
	return store.inner.Delete(ctx, id)
}

func TestPoolRetriesDirtyRotatedToken(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"expires_in":3600
			}`)
		},
	))
	defer tokenServer.Close()
	inner := accountstore.NewMemoryStore()
	if err := inner.Put(context.Background(), accountstore.Account{
		ID:           "profile",
		Name:         "personal",
		RefreshToken: "old-refresh",
		State:        accountstore.StateActive,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	store := &flakyStore{inner: inner, failPuts: 1}
	var logs bytes.Buffer
	pool, err := New(context.Background(), store, codexauth.Config{
		ClientID: "client",
		TokenURL: tokenServer.URL,
	}, &logs)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	if err := pool.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	records, err := inner.List(context.Background())
	if err != nil {
		t.Fatalf("list after failed persistence: %v", err)
	}
	if records[0].RefreshToken != "old-refresh" {
		t.Fatalf("refresh token persisted unexpectedly")
	}
	if !strings.Contains(logs.String(), "will retry") {
		t.Errorf("logs = %q", logs.String())
	}

	pool.refreshAll(context.Background())
	records, err = inner.List(context.Background())
	if err != nil {
		t.Fatalf("list after retry: %v", err)
	}
	if records[0].RefreshToken != "new-refresh" {
		t.Errorf("retried refresh token = %q", records[0].RefreshToken)
	}
}

func TestPoolBalancesSessionsByCallsInFlight(t *testing.T) {
	t.Parallel()

	pool, err := New(context.Background(), accountstore.NewMemoryStore(), codexauth.Config{
		ClientID: "client",
		TokenURL: "https://example.invalid/token",
	}, nil)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := pool.Add(context.Background(), name, oauthpkce.TokenSet{
			AccessToken:  "access-" + name,
			RefreshToken: "refresh-" + name,
		}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	acquire := func(session string) Lease {
		t.Helper()
		lease, err := pool.Acquire(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("acquire %s: %v", session, err)
		}
		return lease
	}

	a1 := acquire("a")
	b1 := acquire("b")
	c1 := acquire("c")
	if a1.AccountName != "first" || b1.AccountName != "second" || c1.AccountName != "first" {
		t.Fatalf("accounts = %s, %s, %s", a1.AccountName, b1.AccountName, c1.AccountName)
	}
	if c1.InFlight != 2 {
		t.Errorf("third lease in flight = %d, want 2", c1.InFlight)
	}

	// Sessions stay on their account while no limit is set, even when loaded.
	a2 := acquire("a")
	if a2.AccountName != "first" || a2.InFlight != 3 {
		t.Errorf("sticky lease = %s with %d in flight", a2.AccountName, a2.InFlight)
	}
	pool.Release(a2.AccountID)

	// A new session goes to the least loaded account, not the next in turn.
	pool.Release(b1.AccountID)
	d1 := acquire("d")
	if d1.AccountName != "second" || d1.InFlight != 1 {
		t.Errorf("new session lease = %s with %d in flight", d1.AccountName, d1.InFlight)
	}

	// At the limit, a session moves to an account below it.
	pool.SetMaxInFlight(2)
	a3 := acquire("a")
	if a3.AccountName != "second" {
		t.Errorf("session at limit stayed on %s", a3.AccountName)
	}
	a4 := acquire("a")
	if a4.AccountName != "second" {
		t.Errorf("moved session did not stay on its new account: %s", a4.AccountName)
	}

	// When every account is at the limit, the session stays where it is.
	e1 := acquire("e")
	if e1.AccountName != "first" {
		t.Errorf("new session at saturation went to %s", e1.AccountName)
	}
	if got := pool.InFlight(); got["first"] != 3 || got["second"] != 3 {
		t.Errorf("in flight = %v", got)
	}
	for _, lease := range []Lease{a1, c1, d1, a3, a4, e1} {
		pool.Release(lease.AccountID)
	}
	if got := pool.InFlight(); got["first"] != 0 || got["second"] != 0 {
		t.Errorf("in flight after release = %v", got)
	}
}

func TestPoolPriorityAndFailoverBalance(t *testing.T) {
	t.Parallel()

	newPool := func(t *testing.T) *Pool {
		t.Helper()
		pool, err := New(context.Background(), accountstore.NewMemoryStore(), codexauth.Config{
			ClientID: "client",
			TokenURL: "https://example.invalid/token",
		}, nil)
		if err != nil {
			t.Fatalf("new pool: %v", err)
		}
		// Added personal first, so store order differs from the configured order.
		for _, name := range []string{"personal", "team"} {
			if _, err := pool.Add(context.Background(), name, oauthpkce.TokenSet{
				AccessToken:  "access-" + name,
				RefreshToken: "refresh-" + name,
			}); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
		}
		return pool
	}
	acquire := func(t *testing.T, pool *Pool, session string) Lease {
		t.Helper()
		lease, err := pool.Acquire(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("acquire %s: %v", session, err)
		}
		return lease
	}

	t.Run("priority overflows at the limit and returns", func(t *testing.T) {
		t.Parallel()
		pool := newPool(t)
		pool.SetMaxInFlight(2)
		if err := pool.SetBalance(BalancePriority, []string{"team", "personal"}); err != nil {
			t.Fatal(err)
		}
		a := acquire(t, pool, "a")
		b := acquire(t, pool, "b")
		c := acquire(t, pool, "c")
		if a.AccountName != "team" || b.AccountName != "team" || c.AccountName != "personal" {
			t.Fatalf("accounts = %s, %s, %s", a.AccountName, b.AccountName, c.AccountName)
		}
		// Team has room again, so the overflowed session goes back to it.
		pool.Release(a.AccountID)
		c2 := acquire(t, pool, "c")
		if c2.AccountName != "team" {
			t.Errorf("session did not return to team: %s", c2.AccountName)
		}
		// Team is full, so new sessions overflow to personal until it is full
		// too. After that, calls still go through to the least loaded account.
		pool.Release(c.AccountID)
		d := acquire(t, pool, "d")
		e := acquire(t, pool, "e")
		if d.AccountName != "personal" || e.AccountName != "personal" {
			t.Errorf("overflow accounts = %s, %s", d.AccountName, e.AccountName)
		}
		f := acquire(t, pool, "f")
		if f.InFlight != 3 {
			t.Errorf("saturated call in flight = %d, want 3", f.InFlight)
		}
	})

	t.Run("failover ignores load and moves on rate limits", func(t *testing.T) {
		t.Parallel()
		pool := newPool(t)
		pool.SetMaxInFlight(1)
		if err := pool.SetBalance(BalanceFailover, []string{"team"}); err != nil {
			t.Fatal(err)
		}
		var team Lease
		for _, session := range []string{"a", "b", "c"} {
			team = acquire(t, pool, session)
			if team.AccountName != "team" {
				t.Fatalf("session %s went to %s while team was usable", session, team.AccountName)
			}
		}
		pool.MarkRateLimited(team.AccountID, time.Now().Add(time.Hour))
		moved := acquire(t, pool, "a")
		if moved.AccountName != "personal" {
			t.Errorf("rate-limited team did not fail over: %s", moved.AccountName)
		}
		pool.mu.Lock()
		pool.accountByIDLocked(team.AccountID).cooldownUntil = time.Time{}
		pool.mu.Unlock()
		back := acquire(t, pool, "a")
		if back.AccountName != "team" {
			t.Errorf("session did not return to recovered team: %s", back.AccountName)
		}
	})

	t.Run("rejects unknown and duplicate names", func(t *testing.T) {
		t.Parallel()
		pool := newPool(t)
		if err := pool.SetBalance(BalancePriority, []string{"missing"}); err == nil {
			t.Error("unknown account name accepted")
		}
		if err := pool.SetBalance(BalanceFailover, []string{"team", "team"}); err == nil {
			t.Error("duplicate account name accepted")
		}
		if _, err := ParseBalance("random"); err == nil {
			t.Error("unknown mode accepted")
		}
	})
}
