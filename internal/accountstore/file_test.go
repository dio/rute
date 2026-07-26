package accountstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestFileStoreRoundTripAndPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	createdAt := time.Date(2026, time.July, 26, 1, 2, 3, 0, time.UTC)
	account := Account{
		ID:           "account-1",
		Name:         "personal",
		AccountID:    "upstream-1",
		RefreshToken: "refresh-secret",
		State:        StateActive,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if err := store.Put(context.Background(), account); err != nil {
		t.Fatalf("put account: %v", err)
	}
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].RefreshToken != "refresh-secret" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if !accounts[0].CreatedAt.Equal(createdAt) {
		t.Errorf("created at = %s, want %s", accounts[0].CreatedAt, createdAt)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("credential mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("state directory mode = %04o, want 0700", got)
	}

	account.UpdatedAt = createdAt.Add(time.Hour)
	account.CreatedAt = time.Time{}
	if err := store.Put(context.Background(), account); err != nil {
		t.Fatalf("update account: %v", err)
	}
	accounts, err = store.List(context.Background())
	if err != nil {
		t.Fatalf("list updated accounts: %v", err)
	}
	if !accounts[0].CreatedAt.Equal(createdAt) {
		t.Errorf("updated account changed created at to %s", accounts[0].CreatedAt)
	}
}

func TestFileStoreAuditHistoryIsRedactedChainedAndFilterable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	account := Account{
		ID:           "codex-record",
		Provider:     ProviderCodex,
		Name:         "personal",
		RefreshToken: "refresh-secret-that-must-not-enter-history",
		State:        StateActive,
	}
	if err := store.Put(context.Background(), account); err != nil {
		t.Fatalf("put account: %v", err)
	}
	for _, event := range []AuditEvent{
		{
			Action:      AuditAccountAdded,
			Provider:    ProviderCodex,
			RecordID:    account.ID,
			AccountName: account.Name,
			State:       StateActive,
		},
		{
			Action:      AuditCredentialsRefreshed,
			Provider:    ProviderClaude,
			RecordID:    "claude-record",
			AccountName: "work",
			State:       StateActive,
		},
	} {
		if err := store.RecordAudit(context.Background(), event); err != nil {
			t.Fatalf("record audit event: %v", err)
		}
	}

	events, err := store.AuditHistory(context.Background(), AuditFilter{})
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].Sequence != 1 || events[0].PreviousHash != "" {
		t.Errorf("first event chain fields = sequence %d, previous %q", events[0].Sequence, events[0].PreviousHash)
	}
	if events[1].Sequence != 2 || events[1].PreviousHash != events[0].Hash {
		t.Errorf("second event does not link to first: %+v", events[1])
	}
	filtered, err := store.AuditHistory(context.Background(), AuditFilter{
		Provider:    ProviderCodex,
		AccountName: "personal",
	})
	if err != nil {
		t.Fatalf("filter audit history: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Action != AuditAccountAdded {
		t.Errorf("filtered events = %+v", filtered)
	}
	body, err := os.ReadFile(store.AuditPath())
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	if strings.Contains(string(body), account.RefreshToken) {
		t.Fatal("refresh token leaked into audit history")
	}
	info, err := os.Stat(store.AuditPath())
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("audit mode = %04o, want 0600", got)
	}
}

func TestFileStoreAuditHistoryRejectsModifiedEvent(t *testing.T) {
	t.Parallel()

	store, err := NewFileStore(filepath.Join(t.TempDir(), "state", "credentials.json"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	if err := store.RecordAudit(context.Background(), AuditEvent{
		Action:      AuditAccountAdded,
		Provider:    ProviderCodex,
		RecordID:    "record",
		AccountName: "personal",
		State:       StateActive,
	}); err != nil {
		t.Fatalf("record audit event: %v", err)
	}
	body, err := os.ReadFile(store.AuditPath())
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	body = []byte(strings.Replace(
		string(body),
		string(AuditAccountAdded),
		string(AuditAccountRemoved),
		1,
	))
	if err := os.WriteFile(store.AuditPath(), body, 0o600); err != nil {
		t.Fatalf("modify audit file: %v", err)
	}
	if _, err := store.AuditHistory(context.Background(), AuditFilter{}); err == nil {
		t.Fatal("modified audit history passed verification")
	}
	if err := store.RecordAudit(context.Background(), AuditEvent{
		Action:      AuditCredentialsRefreshed,
		Provider:    ProviderCodex,
		RecordID:    "record",
		AccountName: "personal",
		State:       StateActive,
	}); err == nil {
		t.Fatal("append to modified audit history succeeded")
	}
}

func TestFileStoreSerializesConcurrentAuditWriters(t *testing.T) {
	t.Parallel()

	store, err := NewFileStore(filepath.Join(t.TempDir(), "state", "credentials.json"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	const writers = 16
	var wait sync.WaitGroup
	wait.Add(writers)
	for index := range writers {
		go func() {
			defer wait.Done()
			suffix := string(rune('a' + index))
			if err := store.RecordAudit(context.Background(), AuditEvent{
				Action:      AuditAccountAdded,
				Provider:    ProviderCodex,
				RecordID:    "record-" + suffix,
				AccountName: "account-" + suffix,
				State:       StateActive,
			}); err != nil {
				t.Errorf("record audit event: %v", err)
			}
		}()
	}
	wait.Wait()
	events, err := store.AuditHistory(context.Background(), AuditFilter{})
	if err != nil {
		t.Fatalf("read audit history: %v", err)
	}
	if len(events) != writers {
		t.Fatalf("event count = %d, want %d", len(events), writers)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) {
			t.Errorf("event %d sequence = %d", index, event.Sequence)
		}
	}
}

func TestFileStoreRejectsSymlinkAndOpenPermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "target.json")
				if err := os.WriteFile(target, []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
					t.Fatalf("write target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("create symlink: %v", err)
				}
			},
		},
		{
			name: "open permissions",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(`{"version":1,"accounts":[]}`), 0o644); err != nil {
					t.Fatalf("write credential file: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "credentials.json")
			test.setup(t, path)
			store, err := NewFileStore(path)
			if err != nil {
				t.Fatalf("new file store: %v", err)
			}
			if _, err := store.List(context.Background()); err == nil {
				t.Fatal("list succeeded, want security error")
			}
		})
	}
}

func TestFileStoreSerializesConcurrentWriters(t *testing.T) {
	t.Parallel()

	store, err := NewFileStore(filepath.Join(t.TempDir(), "state", "credentials.json"))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	const writers = 16
	var wait sync.WaitGroup
	wait.Add(writers)
	for index := range writers {
		go func() {
			defer wait.Done()
			account := Account{
				ID:           "id-" + string(rune('a'+index)),
				Name:         "account-" + string(rune('a'+index)),
				RefreshToken: "refresh",
				State:        StateActive,
			}
			if err := store.Put(context.Background(), account); err != nil {
				t.Errorf("put account: %v", err)
			}
		}()
	}
	wait.Wait()
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != writers {
		t.Errorf("account count = %d, want %d", len(accounts), writers)
	}
}

func TestFileLockHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	path := filepath.Join(dir, "credentials.lock")
	first, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatalf("take first lock: %v", err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second lock error = %v, want deadline exceeded", err)
	}
}

type fakeKeyring struct {
	mu    sync.Mutex
	items map[string]string
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{items: make(map[string]string)}
}

func (ring *fakeKeyring) Get(service, user string) (string, error) {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	value, ok := ring.items[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (ring *fakeKeyring) Set(service, user, password string) error {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	ring.items[service+"\x00"+user] = password
	return nil
}

func (ring *fakeKeyring) Delete(service, user string) error {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	key := service + "\x00" + user
	if _, ok := ring.items[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(ring.items, key)
	return nil
}

func TestKeyringStoreKeepsSecretOutOfManifest(t *testing.T) {
	t.Parallel()

	manifestPath := filepath.Join(t.TempDir(), "state", "accounts.json")
	manifest, err := NewFileStore(manifestPath)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	ring := newFakeKeyring()
	store := newKeyringStore(ring, manifest)
	account := Account{
		ID:           "profile-id",
		Name:         "personal",
		RefreshToken: "very-secret-refresh",
		State:        StateActive,
	}
	if err := store.Put(context.Background(), account); err != nil {
		t.Fatalf("put keyring account: %v", err)
	}
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manifestBody), account.RefreshToken) {
		t.Fatal("refresh token leaked into keyring manifest")
	}
	if err := store.RecordAudit(context.Background(), AuditEvent{
		Action:      AuditAccountAdded,
		Provider:    ProviderCodex,
		RecordID:    account.ID,
		AccountName: account.Name,
		State:       account.State,
	}); err != nil {
		t.Fatalf("record keyring audit event: %v", err)
	}
	events, err := store.AuditHistory(context.Background(), AuditFilter{})
	if err != nil {
		t.Fatalf("read keyring audit history: %v", err)
	}
	if len(events) != 1 || events[0].RecordID != account.ID {
		t.Errorf("keyring audit events = %+v", events)
	}
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list keyring accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].RefreshToken != account.RefreshToken {
		t.Fatalf("accounts = %+v", accounts)
	}
	if err := store.Delete(context.Background(), account.ID); err != nil {
		t.Fatalf("delete keyring account: %v", err)
	}
	if _, err := ring.Get(
		keyringService(ProviderCodex),
		account.ID,
	); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("keyring get after delete error = %v", err)
	}
}

func TestFileStoreMigratesVersionOneRecordsToCodex(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod state directory: %v", err)
	}
	path := filepath.Join(directory, "credentials.json")
	versionOne := `{
	  "version": 1,
	  "accounts": [{
	    "id": "old-id",
	    "name": "default",
	    "refresh_token": "refresh",
	    "state": "active",
	    "created_at": "2026-07-26T00:00:00Z",
	    "updated_at": "2026-07-26T00:00:00Z"
	  }]
	}`
	if err := os.WriteFile(path, []byte(versionOne), 0o600); err != nil {
		t.Fatalf("write version one file: %v", err)
	}
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].Provider != ProviderCodex {
		t.Fatalf("accounts = %+v", accounts)
	}
	if err := store.Put(context.Background(), accounts[0]); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if !strings.Contains(string(body), `"version": 2`) ||
		!strings.Contains(string(body), `"provider": "codex"`) {
		t.Errorf("migrated body = %s", body)
	}
}

func TestMemoryStoreScopesNamesByProvider(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	for _, provider := range []string{ProviderCodex, ProviderClaude} {
		if err := store.Put(context.Background(), Account{
			ID:           provider,
			Provider:     provider,
			Name:         "default",
			RefreshToken: "refresh",
		}); err != nil {
			t.Fatalf("put %s: %v", provider, err)
		}
	}
	accounts, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestKeyringStoreSeparatesProviderServices(t *testing.T) {
	t.Parallel()
	manifest, err := NewFileStore(
		filepath.Join(t.TempDir(), "state", "accounts.json"),
	)
	if err != nil {
		t.Fatalf("new manifest: %v", err)
	}
	ring := newFakeKeyring()
	store := newKeyringStore(ring, manifest)
	for _, provider := range []string{ProviderCodex, ProviderClaude} {
		if err := store.Put(context.Background(), Account{
			ID:           provider + "-id",
			Provider:     provider,
			Name:         "default",
			RefreshToken: provider + "-refresh",
		}); err != nil {
			t.Fatalf("put %s account: %v", provider, err)
		}
	}
	for _, provider := range []string{ProviderCodex, ProviderClaude} {
		token, err := ring.Get(keyringService(provider), provider+"-id")
		if err != nil {
			t.Fatalf("get %s token: %v", provider, err)
		}
		if token != provider+"-refresh" {
			t.Errorf("%s token = %q", provider, token)
		}
	}
}
