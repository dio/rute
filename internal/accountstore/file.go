package accountstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxStoreBytes = 4 << 20

type diskFile struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// FileStore stores complete credential records in a permission-restricted JSON
// file. It must be explicitly selected by the user.
type FileStore struct {
	path string
}

// NewFileStore creates a plaintext file store at path.
func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("accountstore: credential file path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("accountstore: resolve credential file: %w", err)
	}
	return &FileStore{path: path}, nil
}

// Path returns the credential file path.
func (store *FileStore) Path() string {
	return store.path
}

// List loads all accounts.
func (store *FileStore) List(ctx context.Context) ([]Account, error) {
	var accounts []Account
	err := store.withLock(ctx, func() error {
		var err error
		accounts, err = readDiskFile(store.path)
		return err
	})
	return accounts, err
}

// Put creates or replaces an account.
func (store *FileStore) Put(ctx context.Context, account Account) error {
	createdAtMissing := account.CreatedAt.IsZero()
	account, err := normalizeAccount(account)
	if err != nil {
		return err
	}
	return store.withLock(ctx, func() error {
		accounts, err := readDiskFile(store.path)
		if err != nil {
			return err
		}
		found := false
		for index := range accounts {
			if accounts[index].ID == account.ID {
				if createdAtMissing {
					account.CreatedAt = accounts[index].CreatedAt
				}
				accounts[index] = account
				found = true
				continue
			}
			if accounts[index].Provider == account.Provider &&
				accounts[index].Name == account.Name {
				return fmt.Errorf(
					"accountstore: %s account name %q already exists",
					account.Provider,
					account.Name,
				)
			}
		}
		if !found {
			accounts = append(accounts, account)
		}
		sortAccounts(accounts)
		return writeDiskFile(store.path, accounts)
	})
}

// Delete removes an account.
func (store *FileStore) Delete(ctx context.Context, id string) error {
	return store.withLock(ctx, func() error {
		accounts, err := readDiskFile(store.path)
		if err != nil {
			return err
		}
		filtered := accounts[:0]
		found := false
		for _, account := range accounts {
			if account.ID == id {
				found = true
				continue
			}
			filtered = append(filtered, account)
		}
		if !found {
			return ErrNotFound
		}
		return writeDiskFile(store.path, filtered)
	})
}

func (store *FileStore) withLock(ctx context.Context, operation func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(store.path)); err != nil {
		return err
	}
	lock, err := lockFile(ctx, store.path+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}

func readDiskFile(path string) ([]Account, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accountstore: inspect credential file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("accountstore: credential path must be a regular file, not a symlink")
	}
	if info.Size() > maxStoreBytes {
		return nil, errors.New("accountstore: credential file exceeds size limit")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf(
			"accountstore: credential file permissions are %04o; require 0600",
			info.Mode().Perm(),
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("accountstore: open credential file: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxStoreBytes+1))
	decoder.DisallowUnknownFields()
	var data diskFile
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("accountstore: decode credential file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("accountstore: credential file contains trailing data")
	}
	if data.Version != 1 && data.Version != currentVersion {
		return nil, fmt.Errorf("accountstore: unsupported credential file version %d", data.Version)
	}
	ids := make(map[string]struct{}, len(data.Accounts))
	names := make(map[string]struct{}, len(data.Accounts))
	for index, account := range data.Accounts {
		normalized, err := normalizeAccount(account)
		if err != nil {
			return nil, fmt.Errorf("accountstore: invalid account %d: %w", index, err)
		}
		if _, exists := ids[normalized.ID]; exists {
			return nil, fmt.Errorf("accountstore: duplicate account ID %q", normalized.ID)
		}
		nameKey := normalized.Provider + "\x00" + normalized.Name
		if _, exists := names[nameKey]; exists {
			return nil, fmt.Errorf(
				"accountstore: duplicate %s account name %q",
				normalized.Provider,
				normalized.Name,
			)
		}
		ids[normalized.ID] = struct{}{}
		names[nameKey] = struct{}{}
		data.Accounts[index] = normalized
	}
	sortAccounts(data.Accounts)
	return data.Accounts, nil
}

func writeDiskFile(path string, accounts []Account) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".rute-credentials-*")
	if err != nil {
		return fmt.Errorf("accountstore: create temporary credential file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("accountstore: restrict temporary credential file: %w", err)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(diskFile{Version: currentVersion, Accounts: accounts}); err != nil {
		temp.Close()
		return fmt.Errorf("accountstore: encode credential file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("accountstore: sync credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("accountstore: close credential file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("accountstore: replace credential file: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err == nil {
		syncErr := dirHandle.Sync()
		closeErr := dirHandle.Close()
		if syncErr != nil {
			return fmt.Errorf("accountstore: sync credential directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("accountstore: close credential directory: %w", closeErr)
		}
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("accountstore: create state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("accountstore: inspect state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("accountstore: state path must be a directory, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf(
			"accountstore: state directory permissions are %04o; require 0700",
			info.Mode().Perm(),
		)
	}
	return nil
}
