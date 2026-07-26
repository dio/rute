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
