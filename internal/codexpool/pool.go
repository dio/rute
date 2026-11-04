package codexpool

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"slices"
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

// Lease identifies the selected local account and its upstream token. Every
// Lease returned by Acquire must be returned with Release.
type Lease struct {
	AccountID   string
	AccountName string
	// InFlight counts the account's leased calls when this lease was
	// acquired, including this one.
	InFlight int
	Token    codexauth.Token
}

type account struct {
	record              accountstore.Account
	source              *codexauth.Source
	storedRefresh       string
	dirty               bool
	cooldownUntil       time.Time
	cooldownRateLimited bool
	cooldownReason      string
	inFlight            int
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

	mu          sync.Mutex
	accounts    []*account
	sessions    map[string]sessionBinding
	rr          int
	maxInFlight int
	balance     Balance
	// order ranks account names for the priority and failover modes.
	order map[string]int
}

// Balance selects how calls are distributed across a provider's accounts.
type Balance string

// Balance modes.
const (
	// BalanceSpread sends each new session to the account with the fewest
	// calls in flight.
	BalanceSpread Balance = "spread"
	// BalancePriority uses accounts in order, moving to the next account
	// when one is unusable or at the in-flight limit.
	BalancePriority Balance = "priority"
	// BalanceFailover uses accounts in order, moving to the next account
	// only when one is unusable, such as when it is rate-limited.
	BalanceFailover Balance = "failover"
)

// ParseBalance validates a balance mode name.
func ParseBalance(value string) (Balance, error) {
	switch mode := Balance(value); mode {
	case BalanceSpread, BalancePriority, BalanceFailover:
		return mode, nil
	}
	return "", fmt.Errorf("balance must be %s, %s, or %s",
		BalanceSpread, BalancePriority, BalanceFailover)
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
			pool.mu.Lock()
			item.inFlight++
			lease := Lease{
				AccountID:   id,
				AccountName: item.record.Name,
				InFlight:    item.inFlight,
				Token:       token,
			}
			pool.mu.Unlock()
			return lease, nil
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

// Release ends a lease returned by Acquire.
func (pool *Pool) Release(id string) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if item := pool.accountByIDLocked(id); item != nil && item.inFlight > 0 {
		item.inFlight--
	}
}

// SetMaxInFlight sets the per-account number of leased calls above which new
// and existing sessions move to a less loaded account. Zero disables the
// limit; new sessions still go to the least loaded account.
func (pool *Pool) SetMaxInFlight(limit int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.maxInFlight = max(0, limit)
}

// SetBalance sets the distribution mode. For the priority and failover
// modes, names lists accounts from most to least preferred; accounts not
// named follow in store order, and sessions return to a more preferred
// account once it is usable again. Every name must belong to a loaded
// account.
func (pool *Pool) SetBalance(mode Balance, names []string) error {
	if _, err := ParseBalance(string(mode)); err != nil {
		return err
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	order, err := pool.orderLocked(names)
	if err != nil {
		return err
	}
	pool.balance = mode
	pool.order = order
	return nil
}

// Routing errors returned by SetRouting.
var (
	ErrOrderNeedsOrderedBalance = errors.New("an account order requires the priority or failover balance")
	ErrPriorityNeedsLimit       = errors.New("the priority balance requires a max in flight; " +
		"use failover to move only on rate limits")
)

// SetRouting validates and applies a balance mode, account order, and
// per-account in-flight limit together. On error nothing changes.
func (pool *Pool) SetRouting(mode Balance, names []string, limit int) error {
	if _, err := ParseBalance(string(mode)); err != nil {
		return err
	}
	if len(names) > 0 && mode == BalanceSpread {
		return ErrOrderNeedsOrderedBalance
	}
	if limit < 0 {
		return errors.New("max in flight must not be negative")
	}
	if mode == BalancePriority && limit == 0 {
		return ErrPriorityNeedsLimit
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	order, err := pool.orderLocked(names)
	if err != nil {
		return err
	}
	pool.balance = mode
	pool.order = order
	pool.maxInFlight = limit
	return nil
}

func (pool *Pool) orderLocked(names []string) (map[string]int, error) {
	order := make(map[string]int, len(names))
	for index, name := range names {
		if pool.accountByNameLocked(name) == nil {
			return nil, fmt.Errorf("codexpool: %s account %q in account order does not exist", pool.provider, name)
		}
		if _, duplicate := order[name]; duplicate {
			return nil, fmt.Errorf("codexpool: account %q appears twice in account order", name)
		}
		order[name] = index
	}
	return order, nil
}

// rankLocked returns the priority of the account at index; lower is earlier.
func (pool *Pool) rankLocked(item *account, index int) int {
	if rank, ok := pool.order[item.record.Name]; ok {
		return rank
	}
	return len(pool.order) + index
}

// AccountStatus describes one account for status views. It contains no
// credentials.
type AccountStatus struct {
	Name          string             `json:"name"`
	State         accountstore.State `json:"state"`
	InFlight      int                `json:"in_flight"`
	CooldownUntil *time.Time         `json:"cooldown_until,omitempty"`
	RateLimited   bool               `json:"rate_limited,omitempty"`
	Reason        string             `json:"reason,omitempty"`
}

// Status describes a provider's pool for status views.
type Status struct {
	Provider    string          `json:"provider"`
	Balance     Balance         `json:"balance"`
	Order       []string        `json:"order,omitempty"`
	MaxInFlight int             `json:"max_in_flight"`
	Accounts    []AccountStatus `json:"accounts"`
}

// Status returns a snapshot of the pool. Accounts are listed in preference
// order for the priority and failover modes, and in store order otherwise.
func (pool *Pool) Status() Status {
	now := time.Now()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	status := Status{
		Provider:    pool.provider,
		Balance:     pool.balance,
		MaxInFlight: pool.maxInFlight,
		Accounts:    make([]AccountStatus, 0, len(pool.accounts)),
	}
	if status.Balance == "" {
		status.Balance = BalanceSpread
	}
	type ranked struct {
		rank   int
		status AccountStatus
	}
	items := make([]ranked, 0, len(pool.accounts))
	for index, item := range pool.accounts {
		account := AccountStatus{
			Name:     item.record.Name,
			State:    item.record.State,
			InFlight: item.inFlight,
		}
		if item.source == nil {
			account.State = accountstore.StateReauthRequired
		}
		if now.Before(item.cooldownUntil) {
			until := item.cooldownUntil.UTC()
			account.CooldownUntil = &until
			account.RateLimited = item.cooldownRateLimited
			account.Reason = item.cooldownReason
		}
		rank := index
		if status.Balance != BalanceSpread {
			rank = pool.rankLocked(item, index)
		}
		items = append(items, ranked{rank: rank, status: account})
	}
	slices.SortStableFunc(items, func(left, right ranked) int { return left.rank - right.rank })
	for _, item := range items {
		status.Accounts = append(status.Accounts, item.status)
		if status.Balance != BalanceSpread {
			status.Order = append(status.Order, item.status.Name)
		}
	}
	return status
}

// InFlight returns the leased calls for each account name.
func (pool *Pool) InFlight() map[string]int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	counts := make(map[string]int, len(pool.accounts))
	for _, item := range pool.accounts {
		counts[item.record.Name] = item.inFlight
	}
	return counts
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
	pool.mu.Lock()
	lease := Lease{
		AccountID:   id,
		AccountName: item.record.Name,
		InFlight:    item.inFlight,
		Token:       token,
	}
	pool.mu.Unlock()
	return lease, nil
}

// MarkRateLimited cools down an account and unbinds its sessions. reason is a
// short upstream label, such as the exhausted quota window, for status views.
func (pool *Pool) MarkRateLimited(id string, until time.Time, reason string) {
	pool.setCooldownReason(id, until, true)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if item := pool.accountByIDLocked(id); item != nil && item.cooldownRateLimited {
		item.cooldownReason = reason
	}
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
	pool.setCooldownReason(id, until, false)
}

func (pool *Pool) setCooldownReason(id string, until time.Time, rateLimited bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	item := pool.accountByIDLocked(id)
	if item == nil {
		return
	}
	if until.After(item.cooldownUntil) {
		item.cooldownUntil = until
		item.cooldownRateLimited = rateLimited
		item.cooldownReason = ""
	}
	pool.unbindAccountLocked(id)
}

// selectAccount picks an account for one call.
//
// By default it spreads load: a session stays on its bound account unless
// that account is at the in-flight limit while another eligible account is
// below it, and other sessions go to the eligible account with the fewest
// calls in flight, with ties broken in round-robin order.
//
// In priority mode, it takes the most preferred eligible account below the
// in-flight limit, rebinding the session if needed. When every eligible
// account is at the limit, it falls back to the spreading rules. In failover
// mode, it takes the most preferred eligible account regardless of load.
func (pool *Pool) selectAccount(
	sessionID string,
	excluded map[string]struct{},
) (*account, error) {
	now := time.Now()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.cleanupSessionsLocked(now)
	if len(pool.accounts) == 0 {
		return nil, ErrAuthenticationRequired
	}
	var (
		earliest         time.Time
		reauthCount      int
		rateLimitedCount int
		best             *account
		bestIndex        int
		preferred        *account
		preferredRank    int
	)
	for offset := range pool.accounts {
		index := (pool.rr + offset) % len(pool.accounts)
		item := pool.accounts[index]
		if item.record.State == accountstore.StateReauthRequired || item.source == nil {
			reauthCount++
			continue
		}
		if now.Before(item.cooldownUntil) {
			if item.cooldownRateLimited {
				rateLimitedCount++
			}
			if earliest.IsZero() || item.cooldownUntil.Before(earliest) {
				earliest = item.cooldownUntil
			}
			continue
		}
		if _, skip := excluded[item.record.ID]; skip {
			continue
		}
		if best == nil || item.inFlight < best.inFlight {
			best = item
			bestIndex = index
		}
		if pool.balance == BalanceFailover ||
			(pool.balance == BalancePriority &&
				(pool.maxInFlight == 0 || item.inFlight < pool.maxInFlight)) {
			if rank := pool.rankLocked(item, index); preferred == nil || rank < preferredRank {
				preferred = item
				preferredRank = rank
			}
		}
	}
	if preferred != nil {
		pool.bindLocked(sessionID, preferred, now)
		return preferred, nil
	}
	if binding, ok := pool.sessions[sessionID]; ok {
		item := pool.accountByIDLocked(binding.accountID)
		if pool.eligibleLocked(item, now, excluded) &&
			(pool.maxInFlight == 0 ||
				item.inFlight < pool.maxInFlight ||
				best == nil ||
				best.inFlight >= pool.maxInFlight) {
			binding.lastUsed = now
			pool.sessions[sessionID] = binding
			return item, nil
		}
		delete(pool.sessions, sessionID)
	}
	if best != nil {
		pool.rr = (bestIndex + 1) % len(pool.accounts)
		pool.bindLocked(sessionID, best, now)
		return best, nil
	}
	if reauthCount == len(pool.accounts) {
		return nil, ErrAuthenticationRequired
	}
	return nil, &UnavailableError{
		RetryAt:     earliest,
		RateLimited: rateLimitedCount > 0 && rateLimitedCount+reauthCount == len(pool.accounts),
	}
}

func (pool *Pool) bindLocked(sessionID string, item *account, now time.Time) {
	if sessionID != "" {
		pool.sessions[sessionID] = sessionBinding{
			accountID: item.record.ID,
			lastUsed:  now,
		}
	}
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
