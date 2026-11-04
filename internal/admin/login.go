package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/commandcodeauth"
	"github.com/dio/rute/oauthpkce"
	"github.com/dio/rute/providers/claude"
	"github.com/dio/rute/providers/codex"
)

// Login actions.
const (
	ActionAdd    = "add"
	ActionReauth = "reauth"
)

// Login states.
const (
	LoginPending = "pending"
	LoginDone    = "done"
	LoginFailed  = "failed"
)

const maxRetainedLogins = 20

var errLoginInProgress = errors.New("another login is in progress; finish it or wait for it to expire")

// Session is an OAuth authorization in progress.
type Session interface {
	AuthorizationURL() string
	Wait(ctx context.Context) (oauthpkce.TokenSet, error)
	Close() error
}

// APIKeySession is a login that provisions a static API key instead of OAuth
// tokens, as Command Code does.
type APIKeySession interface {
	AuthorizationURL() string
	WaitAPIKey(ctx context.Context) (commandcodeauth.Credentials, error)
	Close() error
}

// Pending is an account login in progress, of either kind. The login manager
// completes it by type.
type Pending interface {
	AuthorizationURL() string
	Close() error
}

// SessionStarter begins an account login for provider.
type SessionStarter func(provider string) (Pending, error)

// StartSession begins the provider's login: an Authorization Code + PKCE flow
// for Codex and Claude, and a browser API-key login for Command Code, as
// `rute account add` does.
func StartSession(provider string) (Pending, error) {
	if provider == accountstore.ProviderCommandCode {
		session, err := commandcodeauth.Start(commandcodeauth.Options{})
		if err != nil {
			return nil, err
		}
		return commandCodeSession{session: session}, nil
	}
	session, err := StartOAuthSession(provider)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// commandCodeSession adapts a Command Code browser login to Pending.
type commandCodeSession struct {
	session *commandcodeauth.Session
}

func (session commandCodeSession) AuthorizationURL() string {
	return session.session.AuthorizationURL()
}

func (session commandCodeSession) WaitAPIKey(ctx context.Context) (commandcodeauth.Credentials, error) {
	return session.session.Wait(ctx)
}

func (session commandCodeSession) Close() error {
	return session.session.Close()
}

// StartOAuthSession begins the provider's Authorization Code + PKCE flow with
// a loopback callback, as `rute account add` does.
func StartOAuthSession(provider string) (Session, error) {
	var config oauthpkce.Config
	switch provider {
	case accountstore.ProviderCodex:
		config = codex.Config()
	case accountstore.ProviderClaude:
		config = claude.Config()
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
	client, err := oauthpkce.New(config)
	if err != nil {
		return nil, err
	}
	return client.Start()
}

// Login describes one account login started from the management server.
type Login struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	Name             string    `json:"name"`
	Action           string    `json:"action"`
	State            string    `json:"state"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	Error            string    `json:"error,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type logins struct {
	pools   map[string]*codexpool.Pool
	start   SessionStarter
	timeout time.Duration

	mu    sync.Mutex
	items map[string]*Login
	order []string
}

// target validates an add or reauth against a provider's pool and returns it.
func (manager *logins) target(provider, name, action string) (*codexpool.Pool, error) {
	pool := manager.pools[provider]
	if pool == nil {
		return nil, fmt.Errorf("provider %q is not available", provider)
	}
	if err := accountstore.ValidateName(name); err != nil {
		return nil, err
	}
	exists := false
	for _, account := range pool.Accounts() {
		if account.Name == name {
			exists = true
		}
	}
	switch action {
	case ActionAdd:
		if exists {
			return nil, fmt.Errorf("%s account %q already exists; reauthenticate it instead", provider, name)
		}
	case ActionReauth:
		if !exists {
			return nil, fmt.Errorf("%s account %q does not exist", provider, name)
		}
	default:
		return nil, fmt.Errorf("action must be %s or %s", ActionAdd, ActionReauth)
	}
	return pool, nil
}

// putKey adds or replaces an API-key account without a browser login.
func (manager *logins) putKey(provider, name, action, apiKey string) (Login, error) {
	pool, err := manager.target(provider, name, action)
	if err != nil {
		return Login{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return Login{}, errors.New("an API key is required")
	}
	ctx := context.Background()
	if action == ActionAdd {
		if _, err := pool.AddAPIKey(ctx, name, apiKey, ""); err != nil {
			return Login{}, err
		}
	} else if err := pool.SetAPIKey(ctx, name, apiKey, ""); err != nil {
		return Login{}, err
	}
	now := time.Now().UTC()
	return Login{
		ID:        randomID(),
		Provider:  provider,
		Name:      name,
		Action:    action,
		State:     LoginDone,
		StartedAt: now,
		ExpiresAt: now,
	}, nil
}

func (manager *logins) begin(provider, name, action string) (Login, error) {
	pool, err := manager.target(provider, name, action)
	if err != nil {
		return Login{}, err
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, item := range manager.items {
		if item.State == LoginPending {
			return Login{}, errLoginInProgress
		}
	}
	session, err := manager.start(provider)
	if err != nil {
		return Login{}, fmt.Errorf("start %s login: %w", provider, err)
	}
	now := time.Now().UTC()
	login := &Login{
		ID:               randomID(),
		Provider:         provider,
		Name:             name,
		Action:           action,
		State:            LoginPending,
		AuthorizationURL: session.AuthorizationURL(),
		StartedAt:        now,
		ExpiresAt:        now.Add(manager.timeout),
	}
	manager.items[login.ID] = login
	manager.order = append(manager.order, login.ID)
	for len(manager.order) > maxRetainedLogins {
		delete(manager.items, manager.order[0])
		manager.order = manager.order[1:]
	}
	go manager.complete(login.ID, pool, session)
	return *login, nil
}

func (manager *logins) complete(id string, pool *codexpool.Pool, pending Pending) {
	defer func() { _ = pending.Close() }()
	manager.mu.Lock()
	login := *manager.items[id]
	manager.mu.Unlock()

	ctx, cancel := context.WithDeadline(context.Background(), login.ExpiresAt)
	defer cancel()

	var err error
	switch session := pending.(type) {
	case APIKeySession:
		var credentials commandcodeauth.Credentials
		credentials, err = session.WaitAPIKey(ctx)
		if err == nil {
			if login.Action == ActionAdd {
				_, err = pool.AddAPIKey(ctx, login.Name, credentials.APIKey, credentials.UserID)
			} else {
				err = pool.SetAPIKey(ctx, login.Name, credentials.APIKey, credentials.UserID)
			}
		}
	case Session:
		var tokens oauthpkce.TokenSet
		tokens, err = session.Wait(ctx)
		if err == nil {
			if login.Action == ActionAdd {
				_, err = pool.Add(ctx, login.Name, tokens)
			} else {
				err = pool.Reauth(ctx, login.Name, tokens)
			}
		}
	default:
		err = errors.New("admin: unsupported login session")
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	item := manager.items[id]
	if item == nil {
		return
	}
	item.AuthorizationURL = ""
	if err != nil {
		item.State = LoginFailed
		item.Error = err.Error()
		return
	}
	item.State = LoginDone
}

func (manager *logins) get(id string) (Login, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	item, ok := manager.items[id]
	if !ok {
		return Login{}, false
	}
	return *item, true
}

func randomID() string {
	var buffer [12]byte
	_, _ = rand.Read(buffer[:])
	return hex.EncodeToString(buffer[:])
}
