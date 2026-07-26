// Package accountstore persists Codex refresh credentials for the local proxy.
package accountstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const currentVersion = 1

var (
	// ErrNotFound indicates that an account does not exist.
	ErrNotFound = errors.New("accountstore: account not found")
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// State describes whether an account can be selected for requests.
type State string

const (
	StateActive         State = "active"
	StateReauthRequired State = "reauth_required"
)

// Account is one persisted Codex account. RefreshToken is always secret.
type Account struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	AccountID    string    `json:"account_id,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	State        State     `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store persists account records.
type Store interface {
	List(ctx context.Context) ([]Account, error)
	Put(ctx context.Context, account Account) error
	Delete(ctx context.Context, id string) error
}

// NewAccountID returns a random opaque identifier suitable for a keyring key.
func NewAccountID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("accountstore: generate account ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// ValidateName validates a user-facing account name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New(
			"account name must start with a letter or digit and contain at most 64 letters, digits, '.', '_', or '-'",
		)
	}
	return nil
}

func normalizeAccount(account Account) (Account, error) {
	account.ID = strings.TrimSpace(account.ID)
	account.Name = strings.TrimSpace(account.Name)
	account.AccountID = strings.TrimSpace(account.AccountID)
	if account.ID == "" {
		return Account{}, errors.New("accountstore: account ID is required")
	}
	if err := ValidateName(account.Name); err != nil {
		return Account{}, fmt.Errorf("accountstore: %w", err)
	}
	if account.State == "" {
		account.State = StateActive
	}
	if account.State != StateActive && account.State != StateReauthRequired {
		return Account{}, fmt.Errorf("accountstore: unsupported account state %q", account.State)
	}
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	if account.UpdatedAt.IsZero() {
		account.UpdatedAt = now
	}
	return account, nil
}

func sortAccounts(accounts []Account) {
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].CreatedAt.Equal(accounts[j].CreatedAt) {
			return accounts[i].Name < accounts[j].Name
		}
		return accounts[i].CreatedAt.Before(accounts[j].CreatedAt)
	})
}

// MemoryStore is a process-local Store.
type MemoryStore struct {
	mu       sync.Mutex
	accounts map[string]Account
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{accounts: make(map[string]Account)}
}

// List returns a copy of the stored accounts.
func (store *MemoryStore) List(ctx context.Context) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	accounts := make([]Account, 0, len(store.accounts))
	for _, account := range store.accounts {
		accounts = append(accounts, account)
	}
	sortAccounts(accounts)
	return accounts, nil
}

// Put creates or replaces an account.
func (store *MemoryStore) Put(ctx context.Context, account Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account, err := normalizeAccount(account)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for id, existing := range store.accounts {
		if id != account.ID && existing.Name == account.Name {
			return fmt.Errorf("accountstore: account name %q already exists", account.Name)
		}
	}
	store.accounts[account.ID] = account
	return nil
}

// Delete removes an account.
func (store *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.accounts[id]; !ok {
		return ErrNotFound
	}
	delete(store.accounts, id)
	return nil
}
