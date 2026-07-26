package accountstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	auditVersion      = 1
	maxAuditBytes     = 16 << 20
	maxAuditLineBytes = 16 << 10
)

// AuditAction describes a persisted credential lifecycle event.
type AuditAction string

const (
	AuditAccountAdded           AuditAction = "account_added"
	AuditCredentialsRefreshed   AuditAction = "credentials_refreshed"
	AuditAccountReauthenticated AuditAction = "account_reauthenticated"
	AuditAccountRemoved         AuditAction = "account_removed"
	AuditReauthRequired         AuditAction = "reauth_required"
)

// AuditEvent is a redacted credential lifecycle event. It intentionally has no
// field capable of carrying OAuth tokens, authorization codes, or verifiers.
type AuditEvent struct {
	Version      int         `json:"version"`
	Sequence     uint64      `json:"sequence"`
	Timestamp    time.Time   `json:"timestamp"`
	Action       AuditAction `json:"action"`
	Provider     string      `json:"provider"`
	RecordID     string      `json:"record_id"`
	AccountName  string      `json:"account_name"`
	State        State       `json:"state,omitempty"`
	PreviousHash string      `json:"previous_hash,omitempty"`
	Hash         string      `json:"hash"`
}

// AuditFilter restricts history by provider and local account name.
type AuditFilter struct {
	Provider    string
	AccountName string
}

type auditableStore interface {
	RecordAudit(ctx context.Context, event AuditEvent) error
	AuditHistory(ctx context.Context, filter AuditFilter) ([]AuditEvent, error)
}

// RecordAudit appends an event when store supports persistent auditing.
// Process-local stores intentionally do not persist history.
func RecordAudit(ctx context.Context, store Store, event AuditEvent) error {
	auditable, ok := store.(auditableStore)
	if !ok {
		return nil
	}
	return auditable.RecordAudit(ctx, event)
}

// AuditHistory returns verified events from a persistent store.
func AuditHistory(
	ctx context.Context,
	store Store,
	filter AuditFilter,
) ([]AuditEvent, error) {
	auditable, ok := store.(auditableStore)
	if !ok {
		return nil, errors.New("accountstore: audit history is unavailable for this store")
	}
	return auditable.AuditHistory(ctx, filter)
}

func (store *FileStore) auditPath() string {
	return store.path + ".audit.jsonl"
}

// AuditPath returns the redacted audit log path associated with the store.
func (store *FileStore) AuditPath() string {
	return store.auditPath()
}

// RecordAudit appends one redacted, hash-chained event.
func (store *FileStore) RecordAudit(ctx context.Context, event AuditEvent) error {
	path := store.auditPath()
	return withAuditLock(ctx, path, func() error {
		events, err := readAuditFile(path)
		if err != nil {
			return err
		}
		event.Version = auditVersion
		event.Sequence = uint64(len(events)) + 1
		event.Timestamp = time.Now().UTC()
		event.PreviousHash = ""
		if len(events) > 0 {
			event.PreviousHash = events[len(events)-1].Hash
		}
		event.Hash = ""
		if err := validateAuditIdentity(event); err != nil {
			return err
		}
		event.Hash, err = auditEventHash(event)
		if err != nil {
			return err
		}
		return appendAuditEvent(path, event)
	})
}

// AuditHistory verifies and filters the complete audit chain.
func (store *FileStore) AuditHistory(
	ctx context.Context,
	filter AuditFilter,
) ([]AuditEvent, error) {
	filter.Provider = strings.TrimSpace(filter.Provider)
	filter.AccountName = strings.TrimSpace(filter.AccountName)
	if filter.Provider != "" && !validProvider(filter.Provider) {
		return nil, fmt.Errorf("accountstore: unsupported provider %q", filter.Provider)
	}
	if filter.AccountName != "" {
		if err := ValidateName(filter.AccountName); err != nil {
			return nil, fmt.Errorf("accountstore: %w", err)
		}
	}
	path := store.auditPath()
	var events []AuditEvent
	err := withAuditLock(ctx, path, func() error {
		var err error
		events, err = readAuditFile(path)
		return err
	})
	if err != nil {
		return nil, err
	}
	filtered := make([]AuditEvent, 0, len(events))
	for _, event := range events {
		if filter.Provider != "" && event.Provider != filter.Provider {
			continue
		}
		if filter.AccountName != "" && event.AccountName != filter.AccountName {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered, nil
}

func withAuditLock(
	ctx context.Context,
	path string,
	operation func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	lock, err := lockFile(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Close()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}

func readAuditFile(path string) ([]AuditEvent, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("accountstore: inspect audit log: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("accountstore: audit path must be a regular file, not a symlink")
	}
	if info.Size() > maxAuditBytes {
		return nil, errors.New("accountstore: audit log exceeds size limit")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf(
			"accountstore: audit log permissions are %04o; require 0600",
			info.Mode().Perm(),
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("accountstore: open audit log: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(io.LimitReader(file, maxAuditBytes+1))
	scanner.Buffer(make([]byte, 4096), maxAuditLineBytes)
	var events []AuditEvent
	var previousHash string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, errors.New("accountstore: audit log contains a blank event")
		}
		var event AuditEvent
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return nil, fmt.Errorf(
				"accountstore: decode audit event %d: %w",
				len(events)+1,
				err,
			)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf(
				"accountstore: audit event %d contains trailing data",
				len(events)+1,
			)
		}
		if err := verifyAuditEvent(event, uint64(len(events)+1), previousHash); err != nil {
			return nil, fmt.Errorf(
				"accountstore: invalid audit event %d: %w",
				len(events)+1,
				err,
			)
		}
		events = append(events, event)
		previousHash = event.Hash
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("accountstore: read audit log: %w", err)
	}
	return events, nil
}

func appendAuditEvent(path string, event AuditEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("accountstore: encode audit event: %w", err)
	}
	if len(body)+1 > maxAuditLineBytes {
		return errors.New("accountstore: audit event exceeds size limit")
	}
	existed := true
	var existingSize int64
	if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		existed = false
	} else if err != nil {
		return fmt.Errorf("accountstore: inspect audit log before append: %w", err)
	} else {
		existingSize = info.Size()
	}
	if existingSize+int64(len(body))+1 > maxAuditBytes {
		return errors.New("accountstore: audit log exceeds size limit")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("accountstore: open audit log for append: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("accountstore: inspect open audit log: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return errors.New("accountstore: open audit log is not a private regular file")
	}
	body = append(body, '\n')
	written, err := file.Write(body)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("accountstore: append audit event: %w", err)
	}
	if written != len(body) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("accountstore: sync audit log: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("accountstore: close audit log: %w", err)
	}
	if !existed {
		dir, err := os.Open(filepath.Dir(path))
		if err == nil {
			syncErr := dir.Sync()
			closeErr := dir.Close()
			if syncErr != nil {
				return fmt.Errorf("accountstore: sync audit directory: %w", syncErr)
			}
			if closeErr != nil {
				return fmt.Errorf("accountstore: close audit directory: %w", closeErr)
			}
		}
	}
	return nil
}

func verifyAuditEvent(event AuditEvent, sequence uint64, previousHash string) error {
	if event.Version != auditVersion {
		return fmt.Errorf("unsupported version %d", event.Version)
	}
	if event.Sequence != sequence {
		return fmt.Errorf("sequence is %d, want %d", event.Sequence, sequence)
	}
	if event.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	if event.PreviousHash != previousHash {
		return errors.New("previous hash does not match")
	}
	if err := validateAuditIdentity(event); err != nil {
		return err
	}
	if !validHash(event.Hash) {
		return errors.New("hash must be a lowercase SHA-256 value")
	}
	want, err := auditEventHash(event)
	if err != nil {
		return err
	}
	if event.Hash != want {
		return errors.New("hash does not match event content")
	}
	return nil
}

func validateAuditIdentity(event AuditEvent) error {
	switch event.Action {
	case AuditAccountAdded,
		AuditCredentialsRefreshed,
		AuditAccountReauthenticated,
		AuditAccountRemoved,
		AuditReauthRequired:
	default:
		return fmt.Errorf("unsupported audit action %q", event.Action)
	}
	if !validProvider(event.Provider) {
		return fmt.Errorf("unsupported provider %q", event.Provider)
	}
	if event.RecordID == "" || len(event.RecordID) > 128 ||
		strings.IndexFunc(event.RecordID, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("record ID must be 1-128 printable characters")
	}
	if err := ValidateName(event.AccountName); err != nil {
		return err
	}
	if event.State != "" &&
		event.State != StateActive &&
		event.State != StateReauthRequired {
		return fmt.Errorf("unsupported account state %q", event.State)
	}
	return nil
}

func validProvider(provider string) bool {
	return provider == ProviderCodex || provider == ProviderClaude
}

func auditEventHash(event AuditEvent) (string, error) {
	payload := struct {
		Version      int         `json:"version"`
		Sequence     uint64      `json:"sequence"`
		Timestamp    string      `json:"timestamp"`
		Action       AuditAction `json:"action"`
		Provider     string      `json:"provider"`
		RecordID     string      `json:"record_id"`
		AccountName  string      `json:"account_name"`
		State        State       `json:"state,omitempty"`
		PreviousHash string      `json:"previous_hash,omitempty"`
	}{
		Version:      event.Version,
		Sequence:     event.Sequence,
		Timestamp:    event.Timestamp.UTC().Format(time.RFC3339Nano),
		Action:       event.Action,
		Provider:     event.Provider,
		RecordID:     event.RecordID,
		AccountName:  event.AccountName,
		State:        event.State,
		PreviousHash: event.PreviousHash,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("accountstore: encode audit hash input: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
