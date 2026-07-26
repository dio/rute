package accountstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

type keyringAPI interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type nativeKeyring struct{}

func (nativeKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (nativeKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (nativeKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// KeyringStore stores refresh tokens in the native keyring and non-secret
// metadata in a private manifest file.
type KeyringStore struct {
	keyring  keyringAPI
	manifest *FileStore
}

// NewKeyringStore creates a native keyring store.
func NewKeyringStore(manifestPath string) (*KeyringStore, error) {
	manifest, err := NewFileStore(manifestPath)
	if err != nil {
		return nil, err
	}
	return &KeyringStore{keyring: nativeKeyring{}, manifest: manifest}, nil
}

func newKeyringStore(keyring keyringAPI, manifest *FileStore) *KeyringStore {
	return &KeyringStore{keyring: keyring, manifest: manifest}
}

// List loads account metadata and resolves every refresh token.
func (store *KeyringStore) List(ctx context.Context) ([]Account, error) {
	accounts, err := store.manifest.List(ctx)
	if err != nil {
		return nil, err
	}
	for index := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := store.keyring.Get(keyringService(accounts[index].Provider), accounts[index].ID)
		if errors.Is(err, keyring.ErrNotFound) {
			accounts[index].State = StateReauthRequired
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"accountstore: read account %q from native keyring: %w",
				accounts[index].Name,
				err,
			)
		}
		accounts[index].RefreshToken = token
	}
	return accounts, ctx.Err()
}

// Put creates or updates an account.
func (store *KeyringStore) Put(ctx context.Context, account Account) error {
	account, err := normalizeAccount(account)
	if err != nil {
		return err
	}
	if account.RefreshToken == "" {
		return errors.New("accountstore: refresh token is required for keyring storage")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	existing, listErr := store.manifest.List(ctx)
	if listErr != nil {
		return listErr
	}
	wasPresent := false
	for _, item := range existing {
		if item.ID == account.ID {
			wasPresent = true
			break
		}
	}
	service := keyringService(account.Provider)
	if err := store.keyring.Set(service, account.ID, account.RefreshToken); err != nil {
		return fmt.Errorf("accountstore: write native keyring: %w", err)
	}
	metadata := account
	metadata.RefreshToken = ""
	if err := store.manifest.Put(ctx, metadata); err != nil {
		if !wasPresent {
			_ = store.keyring.Delete(service, account.ID)
		}
		return err
	}
	return ctx.Err()
}

// Delete removes an account's secret and metadata.
func (store *KeyringStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	accounts, err := store.manifest.List(ctx)
	if err != nil {
		return err
	}
	var provider string
	for _, account := range accounts {
		if account.ID == id {
			provider = account.Provider
			break
		}
	}
	if provider == "" {
		return ErrNotFound
	}
	err = store.keyring.Delete(keyringService(provider), id)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("accountstore: delete native keyring item: %w", err)
	}
	return store.manifest.Delete(ctx, id)
}

// RecordAudit appends an event beside the redacted keyring manifest.
func (store *KeyringStore) RecordAudit(ctx context.Context, event AuditEvent) error {
	return store.manifest.RecordAudit(ctx, event)
}

// AuditHistory returns verified events beside the redacted keyring manifest.
func (store *KeyringStore) AuditHistory(
	ctx context.Context,
	filter AuditFilter,
) ([]AuditEvent, error) {
	return store.manifest.AuditHistory(ctx, filter)
}

func keyringService(provider string) string {
	if provider == "" {
		provider = ProviderCodex
	}
	return "rute/" + provider
}
