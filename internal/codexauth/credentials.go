// Package codexauth manages Codex OAuth credential lifecycles.
package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/dio/rute/oauthpkce"
)

const (
	defaultRefreshBefore  = 5 * time.Minute
	defaultRefreshTimeout = 30 * time.Second
	maxRefreshResponse    = 1 << 20
)

var ErrReauthenticationRequired = errors.New("codexauth: reauthentication required")

// Token is an immutable credential snapshot. Every string field except
// AccountID must be treated as a secret.
type Token struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Expiry       time.Time
}

// Config configures a Source.
type Config struct {
	ClientID      string
	TokenURL      string
	HTTPClient    *http.Client
	RefreshBefore time.Duration
}

// Source serves concurrent credential readers and coalesces refreshes.
type Source struct {
	config Config

	mu         sync.Mutex
	token      Token
	refreshing chan struct{}
	refreshErr error
}

// New creates an in-memory credential source.
func New(cfg Config, tokens oauthpkce.TokenSet) (*Source, error) {
	return newSource(cfg, Token{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		AccountID:    accountID(tokens.IDToken, tokens.AccessToken),
		Expiry:       tokens.Expiry,
	})
}

// NewFromRefreshToken creates a source that exchanges refreshToken on its
// first Get call. accountIDValue is retained when the refresh response does
// not contain account metadata.
func NewFromRefreshToken(
	cfg Config,
	refreshToken string,
	accountIDValue string,
) (*Source, error) {
	return newSource(cfg, Token{
		RefreshToken: refreshToken,
		AccountID:    accountIDValue,
	})
}

func newSource(cfg Config, token Token) (*Source, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("codexauth: client ID is required")
	}
	if cfg.TokenURL == "" {
		return nil, errors.New("codexauth: token URL is required")
	}
	if token.AccessToken == "" && token.RefreshToken == "" {
		return nil, errors.New("codexauth: access token or refresh token is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: defaultRefreshTimeout,
		}
	}
	if cfg.RefreshBefore <= 0 {
		cfg.RefreshBefore = defaultRefreshBefore
	}

	source := &Source{
		config: cfg,
		token:  token,
	}
	return source, nil
}

// Snapshot returns the current immutable token snapshot without refreshing it.
func (source *Source) Snapshot() Token {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.token
}

// Get returns a usable credential snapshot, refreshing it when it is close to
// expiry.
func (source *Source) Get(ctx context.Context) (Token, error) {
	return source.get(ctx, false)
}

// ForceRefresh refreshes credentials even when the current token has not
// reached its refresh window.
func (source *Source) ForceRefresh(ctx context.Context) (Token, error) {
	return source.get(ctx, true)
}

func (source *Source) get(ctx context.Context, force bool) (Token, error) {
	for {
		source.mu.Lock()
		current := source.token
		if !force && tokenUsable(current, source.config.RefreshBefore) {
			source.mu.Unlock()
			return current, nil
		}
		if current.RefreshToken == "" {
			source.mu.Unlock()
			if force {
				return Token{}, ErrReauthenticationRequired
			}
			if current.Expiry.IsZero() || time.Now().Before(current.Expiry) {
				return current, nil
			}
			return Token{}, ErrReauthenticationRequired
		}
		if source.refreshing != nil {
			wait := source.refreshing
			source.mu.Unlock()
			select {
			case <-wait:
				source.mu.Lock()
				latest := source.token
				refreshErr := source.refreshErr
				source.mu.Unlock()
				if refreshErr == nil {
					return latest, nil
				}
				if !force && usableFallback(latest) {
					return latest, nil
				}
				return Token{}, refreshErr
			case <-ctx.Done():
				return Token{}, ctx.Err()
			}
		}

		wait := make(chan struct{})
		source.refreshing = wait
		source.mu.Unlock()

		refreshed, err := source.refresh(ctx, current)

		source.mu.Lock()
		if err == nil {
			source.token = refreshed
		}
		source.refreshErr = err
		source.refreshing = nil
		close(wait)
		latest := source.token
		source.mu.Unlock()

		if err == nil {
			return latest, nil
		}
		if !force && usableFallback(latest) {
			return latest, nil
		}
		return Token{}, err
	}
}

func usableFallback(token Token) bool {
	return token.AccessToken != "" &&
		(token.Expiry.IsZero() || time.Now().Before(token.Expiry))
}

func (source *Source) refresh(ctx context.Context, current Token) (Token, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id":     source.config.ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": current.RefreshToken,
	})
	if err != nil {
		return Token{}, fmt.Errorf("codexauth: encode refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		source.config.TokenURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return Token{}, fmt.Errorf("codexauth: create refresh request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	response, err := source.config.HTTPClient.Do(request)
	if err != nil {
		return Token{}, fmt.Errorf("codexauth: refresh request: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxRefreshResponse+1))
	if err != nil {
		return Token{}, fmt.Errorf("codexauth: read refresh response: %w", err)
	}
	if len(body) > maxRefreshResponse {
		return Token{}, errors.New("codexauth: refresh response exceeds size limit")
	}

	var result refreshResponse
	if err := json.Unmarshal(body, &result); err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Token{}, fmt.Errorf("codexauth: refresh rejected with status %d", response.StatusCode)
		}
		return Token{}, errors.New("codexauth: refresh response is not valid JSON")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if permanentRefreshError(result.Error, result.ErrorDescription) {
			return Token{}, ErrReauthenticationRequired
		}
		return Token{}, fmt.Errorf("codexauth: refresh rejected with status %d", response.StatusCode)
	}
	if result.AccessToken == "" {
		return Token{}, errors.New("codexauth: refresh response omitted access token")
	}

	refreshToken := result.RefreshToken
	if refreshToken == "" {
		refreshToken = current.RefreshToken
	}
	idToken := result.IDToken
	if idToken == "" {
		idToken = current.IDToken
	}
	expiry := time.Time{}
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	account := accountID(idToken, result.AccessToken)
	if account == "" {
		account = current.AccountID
	}
	return Token{
		AccessToken:  result.AccessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		AccountID:    account,
		Expiry:       expiry,
	}, nil
}

func tokenUsable(token Token, refreshBefore time.Duration) bool {
	if token.AccessToken == "" {
		return false
	}
	if token.Expiry.IsZero() {
		return true
	}
	return time.Now().Add(refreshBefore).Before(token.Expiry)
}

func permanentRefreshError(code, description string) bool {
	combined := code + " " + description
	for _, marker := range []string{
		"refresh_token_expired",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"invalid_grant",
	} {
		if bytes.Contains(bytes.ToLower([]byte(combined)), []byte(marker)) {
			return true
		}
	}
	return false
}

type refreshResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}
