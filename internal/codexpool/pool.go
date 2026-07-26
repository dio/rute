package codexpool

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sync"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/oauthpkce"
)

const (
	sessionIdleTTL       = 24 * time.Hour
	maxSessionBindings   = 10_000
	transientCooldown    = 15 * time.Second
	refreshInterval      = 30 * time.Second
	refreshConcurrency   = 2
	persistRetryInterval = 30 * time.Second
	auditWriteTimeout    = 5 * time.Second
	defaultRefreshBefore = 5 * time.Minute
	maxRefreshJitter     = 30 * time.Second
)

// Lease identifies the selected local account and its upstream token.
type Lease struct {
	AccountID string
	Token     codexauth.Token
}

type account struct {
	record        accountstore.Account
	source        *codexauth.Source
	storedRefresh string
	dirty         bool
	cooldownUntil time.Time
}

type sessionBinding struct {
	accountID string
	lastUsed  time.Time
}

type accountSnapshot struct {
	item   *account
	id     string
	source *codexauth.Source
	state  accountstore.State
}

// Pool manages account selection, refreshing, and persistence.
type Pool struct {
	store      accountstore.Store
	provider   string
	authConfig codexauth.Config
	stderr     io.Writer

	mu       sync.Mutex
	accounts []*account
	sessions map[string]sessionBinding
	rr       int
}

// New loads accounts from store. Refresh-only accounts are exchanged lazily
// during Bootstrap or their first request.
func New(
	ctx context.Context,
	store accountstore.Store,
	authConfig codexauth.Config,
	stderr io.Writer,
) (*Pool, error) {
	return NewProvider(
		ctx,
		store,
		accountstore.ProviderCodex,
		authConfig,
		stderr,
	)
}

// NewProvider loads accounts belonging to provider from store.
func NewProvider(
	ctx context.Context,
	store accountstore.Store,
	provider string,
	authConfig codexauth.Config,
	stderr io.Writer,
) (*Pool, error) {
	if store == nil {
		return nil, errors.New("codexpool: store is required")
	}
	if provider != accountstore.ProviderCodex && provider != accountstore.ProviderClaude {
		return nil, fmt.Errorf("codexpool: unsupported provider %q", provider)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	records, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	pool := &Pool{
		store:      store,
		provider:   provider,
		authConfig: authConfig,
		stderr:     stderr,
		sessions:   make(map[string]sessionBinding),
	}
	for _, record := range records {
		if record.Provider != provider {
			continue
		}
		if record.RefreshToken == "" {
			record.State = accountstore.StateReauthRequired
			pool.accounts = append(pool.accounts, &account{record: record})
			continue
		}
		source, err := codexauth.NewFromRefreshToken(
			jitteredAuthConfig(authConfig, record.ID),
			record.RefreshToken,
			record.AccountID,
		)
		if err != nil {
			return nil, fmt.Errorf("codexpool: load account %q: %w", record.Name, err)
		}
		pool.accounts = append(pool.accounts, &account{
			record:        record,
			source:        source,
			storedRefresh: record.RefreshToken,
		})
	}
	return pool, nil
}

// NewEphemeral creates a single-account pool without persistent storage.
func NewEphemeral(
	authConfig codexauth.Config,
	tokens oauthpkce.TokenSet,
	stderr io.Writer,
) (*Pool, error) {
	source, err := codexauth.New(jitteredAuthConfig(authConfig, "environment"), tokens)
	if err != nil {
		return nil, err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	token := source.Snapshot()
	now := time.Now().UTC()
	return &Pool{
		store:      accountstore.NewMemoryStore(),
		provider:   accountstore.ProviderCodex,
		authConfig: authConfig,
		stderr:     stderr,
		accounts: []*account{{
			record: accountstore.Account{
				ID:           "environment",
				Provider:     accountstore.ProviderCodex,
				Name:         "environment",
				AccountID:    token.AccountID,
				RefreshToken: token.RefreshToken,
				State:        accountstore.StateActive,
				CreatedAt:    now,
				UpdatedAt:    now,
			},
			source:        source,
			storedRefresh: token.RefreshToken,
		}},
		sessions: make(map[string]sessionBinding),
	}, nil
}

// Add inserts a newly authorized account into the running pool and store.
func (pool *Pool) Add(
	ctx context.Context,
	name string,
	tokens oauthpkce.TokenSet,
) (accountstore.Account, error) {
	if err := accountstore.ValidateName(name); err != nil {
		return accountstore.Account{}, err
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return accountstore.Account{}, errors.New(
			"codexpool: authorization response must include access and refresh tokens",
		)
	}
	source, err := codexauth.New(jitteredAuthConfig(pool.authConfig, name), tokens)
	if err != nil {
		return accountstore.Account{}, err
	}
	token := source.Snapshot()
	pool.mu.Lock()
	for _, existing := range pool.accounts {
		if existing.record.Name == name {
			pool.mu.Unlock()
			return accountstore.Account{}, fmt.Errorf("codexpool: account name %q already exists", name)
		}
		if token.AccountID != "" && existing.record.AccountID == token.AccountID {
			pool.mu.Unlock()
			return accountstore.Account{}, fmt.Errorf(
				"codexpool: upstream account already exists as %q; use account reauth",
				existing.record.Name,
			)
		}
	}
	pool.mu.Unlock()
	id, err := accountstore.NewAccountID()
	if err != nil {
		return accountstore.Account{}, err
	}
	now := time.Now().UTC()
	record := accountstore.Account{
		ID:           id,
		Provider:     pool.provider,
		Name:         name,
		AccountID:    token.AccountID,
		RefreshToken: token.RefreshToken,
		State:        accountstore.StateActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := pool.store.Put(ctx, record); err != nil {
		return accountstore.Account{}, err
	}
	pool.recordAudit(ctx, accountstore.AuditEvent{
		Action:      accountstore.AuditAccountAdded,
		Provider:    record.Provider,
		RecordID:    record.ID,
		AccountName: record.Name,
		State:       record.State,
	})
	pool.mu.Lock()
	pool.accounts = append(pool.accounts, &account{
		record:        record,
		source:        source,
		storedRefresh: record.RefreshToken,
	})
	pool.mu.Unlock()
	return record, nil
}

// Reauth replaces an account's OAuth credentials after verifying that the
// browser session returned the same upstream account.
func (pool *Pool) Reauth(
	ctx context.Context,
	name string,
	tokens oauthpkce.TokenSet,
) error {
	source, err := codexauth.New(jitteredAuthConfig(pool.authConfig, name), tokens)
	if err != nil {
		return err
	}
	token := source.Snapshot()
	pool.mu.Lock()
	target := pool.accountByNameLocked(name)
	if target == nil {
		pool.mu.Unlock()
		return accountstore.ErrNotFound
	}
	if target.record.AccountID != "" &&
		token.AccountID != "" &&
		target.record.AccountID != token.AccountID {
		pool.mu.Unlock()
		return errors.New(
			"codexpool: browser login returned a different upstream account; use account add",
		)
	}
	record := target.record
	pool.mu.Unlock()

	record.RefreshToken = token.RefreshToken
	if token.AccountID != "" {
		record.AccountID = token.AccountID
	}
	record.State = accountstore.StateActive
	record.UpdatedAt = time.Now().UTC()
	if err := pool.store.Put(ctx, record); err != nil {
		return err
	}
	pool.recordAudit(ctx, accountstore.AuditEvent{
		Action:      accountstore.AuditAccountReauthenticated,
		Provider:    record.Provider,
		RecordID:    record.ID,
		AccountName: record.Name,
		State:       record.State,
	})
	pool.mu.Lock()
	target.record = record
	target.source = source
	target.storedRefresh = token.RefreshToken
	target.dirty = false
	target.cooldownUntil = time.Time{}
	pool.unbindAccountLocked(record.ID)
	pool.mu.Unlock()
	return nil
}

// Delete removes an account by name.
func (pool *Pool) Delete(ctx context.Context, name string) error {
	pool.mu.Lock()
	target := pool.accountByNameLocked(name)
	if target == nil {
		pool.mu.Unlock()
		return accountstore.ErrNotFound
	}
	record := target.record
	pool.mu.Unlock()
	if err := pool.store.Delete(ctx, record.ID); err != nil {
		return err
	}
	pool.recordAudit(ctx, accountstore.AuditEvent{
		Action:      accountstore.AuditAccountRemoved,
		Provider:    record.Provider,
		RecordID:    record.ID,
		AccountName: record.Name,
		State:       record.State,
	})
	pool.mu.Lock()
	for index, candidate := range pool.accounts {
		if candidate.record.ID == record.ID {
			pool.accounts = append(pool.accounts[:index], pool.accounts[index+1:]...)
			break
		}
	}
	pool.unbindAccountLocked(record.ID)
	pool.mu.Unlock()
	return nil
}

// Accounts returns redacted account metadata.
func (pool *Pool) Accounts() []accountstore.Account {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	records := make([]accountstore.Account, 0, len(pool.accounts))
	for _, item := range pool.accounts {
		record := item.record
		record.RefreshToken = ""
		records = append(records, record)
	}
	return records
}

// Bootstrap exchanges persisted refresh tokens. It succeeds when at least one
// account is usable, or when the store is empty.
func (pool *Pool) Bootstrap(ctx context.Context) error {
	accounts := pool.snapshotAccounts()
	if len(accounts) == 0 {
		return nil
	}
	var usable int
	var lastErr error
	for _, snapshot := range accounts {
		if snapshot.source == nil || snapshot.state == accountstore.StateReauthRequired {
			continue
		}
		token, err := snapshot.source.Get(ctx)
		if err != nil {
			lastErr = err
			if errors.Is(err, codexauth.ErrReauthenticationRequired) {
				pool.markReauthRequired(ctx, snapshot.item)
			} else {
				pool.setCooldown(snapshot.id, time.Now().Add(transientCooldown))
			}
			continue
		}
		usable++
		pool.persistToken(ctx, snapshot.item, token)
	}
	if usable > 0 {
		return nil
	}
	if pool.allReauthRequired() {
		return ErrAuthenticationRequired
	}
	if lastErr != nil {
		return fmt.Errorf("codexpool: bootstrap credentials: %w", lastErr)
	}
	return ErrAccountsUnavailable
}

// Acquire selects a session-sticky usable account, excluding IDs already tried
// by the current request.
func (pool *Pool) Acquire(
	ctx context.Context,
	sessionID string,
	excluded map[string]struct{},
) (Lease, error) {
	localExcluded := make(map[string]struct{}, len(excluded)+1)
	for id := range excluded {
		localExcluded[id] = struct{}{}
	}
	for {
		item, err := pool.selectAccount(sessionID, localExcluded)
		if err != nil {
			return Lease{}, err
		}
		pool.mu.Lock()
		id := item.record.ID
		source := item.source
		pool.mu.Unlock()
		token, err := source.Get(ctx)
		if err == nil {
			pool.persistToken(ctx, item, token)
			return Lease{AccountID: id, Token: token}, nil
		}
		localExcluded[id] = struct{}{}
		if errors.Is(err, codexauth.ErrReauthenticationRequired) {
			pool.markReauthRequired(ctx, item)
			continue
		}
		pool.setCooldown(id, time.Now().Add(transientCooldown))
		if ctx.Err() != nil {
			return Lease{}, ctx.Err()
		}
	}
}

// ForceRefresh refreshes the selected account once.
func (pool *Pool) ForceRefresh(ctx context.Context, id string) (Lease, error) {
	pool.mu.Lock()
	item := pool.accountByIDLocked(id)
	var source *codexauth.Source
	if item != nil {
		source = item.source
	}
	pool.mu.Unlock()
	if item == nil || source == nil {
		return Lease{}, accountstore.ErrNotFound
	}
	token, err := source.ForceRefresh(ctx)
	if err != nil {
		if errors.Is(err, codexauth.ErrReauthenticationRequired) {
			pool.markReauthRequired(ctx, item)
		} else {
			pool.setCooldown(id, time.Now().Add(transientCooldown))
		}
		return Lease{}, err
	}
	pool.persistToken(ctx, item, token)
	return Lease{AccountID: id, Token: token}, nil
}

// MarkRateLimited cools down an account and unbinds its sessions.
func (pool *Pool) MarkRateLimited(id string, until time.Time) {
	pool.setCooldown(id, until)
}

// MarkAuthorizationFailure applies a short cooldown after a refreshed token is
// still rejected.
func (pool *Pool) MarkAuthorizationFailure(id string) {
	pool.setCooldown(id, time.Now().Add(transientCooldown))
}

// Run proactively refreshes accounts and retries dirty credential writes until
// ctx is cancelled.
func (pool *Pool) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	pool.refreshAll(ctx)
	for {
		select {
		case <-ctx.Done():
			pool.flushDirty(context.WithoutCancel(ctx))
			return
		case <-ticker.C:
			pool.refreshAll(ctx)
		}
	}
}

func (pool *Pool) refreshAll(ctx context.Context) {
	accounts := pool.snapshotAccounts()
	semaphore := make(chan struct{}, refreshConcurrency)
	var wait sync.WaitGroup
	for _, snapshot := range accounts {
		if snapshot.source == nil || snapshot.state == accountstore.StateReauthRequired {
			continue
		}
		wait.Add(1)
		go func(snapshot accountSnapshot) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			token, err := snapshot.source.Get(ctx)
			if err != nil {
				if errors.Is(err, codexauth.ErrReauthenticationRequired) {
					pool.markReauthRequired(ctx, snapshot.item)
				}
				return
			}
			pool.persistToken(ctx, snapshot.item, token)
		}(snapshot)
	}
	wait.Wait()
}

func (pool *Pool) flushDirty(ctx context.Context) {
	flushCtx, cancel := context.WithTimeout(ctx, persistRetryInterval)
	defer cancel()
	pool.mu.Lock()
	accounts := append([]*account(nil), pool.accounts...)
	pool.mu.Unlock()
	for _, item := range accounts {
		pool.mu.Lock()
		source := item.source
		dirty := item.dirty
		pool.mu.Unlock()
		if source != nil && dirty {
			pool.persistToken(flushCtx, item, source.Snapshot())
		}
	}
}

func (pool *Pool) persistToken(
	ctx context.Context,
	item *account,
	token codexauth.Token,
) {
	pool.mu.Lock()
	if token.RefreshToken == "" ||
		(token.RefreshToken == item.storedRefresh && !item.dirty) {
		pool.mu.Unlock()
		return
	}
	record := item.record
	record.RefreshToken = token.RefreshToken
	if token.AccountID != "" {
		record.AccountID = token.AccountID
	}
	record.State = accountstore.StateActive
	record.UpdatedAt = time.Now().UTC()
	pool.mu.Unlock()

	if err := pool.store.Put(ctx, record); err != nil {
		pool.mu.Lock()
		item.dirty = true
		pool.mu.Unlock()
		fmt.Fprintf(
			pool.stderr,
			"rute proxy: could not persist refreshed credentials for account %q; will retry: %v\n",
			record.Name,
			err,
		)
		return
	}
	pool.mu.Lock()
	item.record = record
	item.storedRefresh = token.RefreshToken
	item.dirty = false
	pool.mu.Unlock()
	pool.recordAudit(ctx, accountstore.AuditEvent{
		Action:      accountstore.AuditCredentialsRefreshed,
		Provider:    record.Provider,
		RecordID:    record.ID,
		AccountName: record.Name,
		State:       record.State,
	})
}

func (pool *Pool) markReauthRequired(ctx context.Context, item *account) {
	pool.mu.Lock()
	record := item.record
	record.State = accountstore.StateReauthRequired
	record.UpdatedAt = time.Now().UTC()
	record.RefreshToken = item.storedRefresh
	item.record = record
	item.cooldownUntil = time.Time{}
	pool.unbindAccountLocked(record.ID)
	pool.mu.Unlock()
	if record.RefreshToken != "" {
		if err := pool.store.Put(context.WithoutCancel(ctx), record); err != nil {
			fmt.Fprintf(
				pool.stderr,
				"rute proxy: could not persist reauthentication state for account %q: %v\n",
				record.Name,
				err,
			)
			return
		}
		pool.recordAudit(ctx, accountstore.AuditEvent{
			Action:      accountstore.AuditReauthRequired,
			Provider:    record.Provider,
			RecordID:    record.ID,
			AccountName: record.Name,
			State:       record.State,
		})
	}
}

func (pool *Pool) recordAudit(ctx context.Context, event accountstore.AuditEvent) {
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := accountstore.RecordAudit(auditCtx, pool.store, event); err != nil {
		_, _ = fmt.Fprintf(
			pool.stderr,
			"rute proxy: credential change succeeded but could not append %q audit event "+
				"for %s account %q: %v\n",
			event.Action,
			event.Provider,
			event.AccountName,
			err,
		)
	}
}

func (pool *Pool) setCooldown(id string, until time.Time) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	item := pool.accountByIDLocked(id)
	if item == nil {
		return
	}
	if until.After(item.cooldownUntil) {
		item.cooldownUntil = until
	}
	pool.unbindAccountLocked(id)
}

func (pool *Pool) selectAccount(
	sessionID string,
	excluded map[string]struct{},
) (*account, error) {
	now := time.Now()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.cleanupSessionsLocked(now)
	if binding, ok := pool.sessions[sessionID]; ok {
		if item := pool.accountByIDLocked(binding.accountID); pool.eligibleLocked(item, now, excluded) {
			binding.lastUsed = now
			pool.sessions[sessionID] = binding
			return item, nil
		}
		delete(pool.sessions, sessionID)
	}
	if len(pool.accounts) == 0 {
		return nil, ErrAuthenticationRequired
	}
	var earliest time.Time
	reauthCount := 0
	for offset := range pool.accounts {
		index := (pool.rr + offset) % len(pool.accounts)
		item := pool.accounts[index]
		if item.record.State == accountstore.StateReauthRequired || item.source == nil {
			reauthCount++
			continue
		}
		if now.Before(item.cooldownUntil) {
			if earliest.IsZero() || item.cooldownUntil.Before(earliest) {
				earliest = item.cooldownUntil
			}
			continue
		}
		if _, skip := excluded[item.record.ID]; skip {
			continue
		}
		pool.rr = (index + 1) % len(pool.accounts)
		if sessionID != "" {
			pool.sessions[sessionID] = sessionBinding{
				accountID: item.record.ID,
				lastUsed:  now,
			}
		}
		return item, nil
	}
	if reauthCount == len(pool.accounts) {
		return nil, ErrAuthenticationRequired
	}
	return nil, &UnavailableError{RetryAt: earliest}
}

func (pool *Pool) eligibleLocked(
	item *account,
	now time.Time,
	excluded map[string]struct{},
) bool {
	if item == nil || item.source == nil ||
		item.record.State == accountstore.StateReauthRequired ||
		now.Before(item.cooldownUntil) {
		return false
	}
	_, skip := excluded[item.record.ID]
	return !skip
}

func (pool *Pool) cleanupSessionsLocked(now time.Time) {
	for id, binding := range pool.sessions {
		if now.Sub(binding.lastUsed) > sessionIdleTTL {
			delete(pool.sessions, id)
		}
	}
	for len(pool.sessions) >= maxSessionBindings {
		var oldestID string
		var oldest time.Time
		for id, binding := range pool.sessions {
			if oldestID == "" || binding.lastUsed.Before(oldest) {
				oldestID = id
				oldest = binding.lastUsed
			}
		}
		delete(pool.sessions, oldestID)
	}
}

func (pool *Pool) unbindAccountLocked(id string) {
	for sessionID, binding := range pool.sessions {
		if binding.accountID == id {
			delete(pool.sessions, sessionID)
		}
	}
}

func (pool *Pool) accountByIDLocked(id string) *account {
	for _, item := range pool.accounts {
		if item.record.ID == id {
			return item
		}
	}
	return nil
}

func (pool *Pool) accountByNameLocked(name string) *account {
	for _, item := range pool.accounts {
		if item.record.Name == name {
			return item
		}
	}
	return nil
}

func (pool *Pool) allReauthRequired() bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.accounts) == 0 {
		return false
	}
	for _, item := range pool.accounts {
		if item.record.State != accountstore.StateReauthRequired && item.source != nil {
			return false
		}
	}
	return true
}

func (pool *Pool) snapshotAccounts() []accountSnapshot {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	snapshots := make([]accountSnapshot, 0, len(pool.accounts))
	for _, item := range pool.accounts {
		snapshots = append(snapshots, accountSnapshot{
			item:   item,
			id:     item.record.ID,
			source: item.source,
			state:  item.record.State,
		})
	}
	return snapshots
}

func jitteredAuthConfig(config codexauth.Config, key string) codexauth.Config {
	refreshBefore := config.RefreshBefore
	if refreshBefore <= 0 {
		refreshBefore = defaultRefreshBefore
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	jitter := time.Duration(hash.Sum32()%uint32(maxRefreshJitter/time.Second)) * time.Second
	if jitter >= refreshBefore {
		jitter = refreshBefore / 2
	}
	config.RefreshBefore = refreshBefore - jitter
	return config
}
