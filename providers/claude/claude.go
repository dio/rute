// Package claude provides the OAuth public-client profile used by Claude Code.
package claude

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dio/rute/oauthpkce"
)

const (
	ClientID         = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AuthorizationURL = "https://claude.ai/oauth/authorize"
	TokenURL         = "https://api.anthropic.com/v1/oauth/token"
	MessagesURL      = "https://api.anthropic.com/v1/messages?beta=true"
	APIVersion       = "2023-06-01"
	OAuthCapability  = "oauth-2025-04-20"

	maxTokenResponse = 1 << 20
)

// Config returns Claude's Authorization Code + PKCE configuration.
func Config() oauthpkce.Config {
	return oauthpkce.Config{
		ClientID:         ClientID,
		AuthorizationURL: AuthorizationURL,
		TokenURL:         TokenURL,
		Scopes: []string{
			"org:create_api_key",
			"user:profile",
			"user:inference",
		},
		AuthorizationParams: url.Values{"code": {"true"}},
		Loopback: oauthpkce.LoopbackConfig{
			RedirectHost: "localhost",
			CallbackPath: "/callback",
		},
		TokenExchanger: jsonExchanger{},
	}
}

type jsonExchanger struct{}

func (jsonExchanger) Exchange(
	ctx context.Context,
	client *http.Client,
	request oauthpkce.ExchangeRequest,
) (oauthpkce.TokenSet, error) {
	code := request.Code
	state := request.State
	if before, after, found := strings.Cut(code, "#"); found {
		code = before
		if !constantTimeEqual(after, state) {
			return oauthpkce.TokenSet{}, errors.New("claude authorization code state mismatch")
		}
		state = after
	}
	payload, err := json.Marshal(map[string]string{
		"code":          code,
		"state":         state,
		"grant_type":    "authorization_code",
		"client_id":     request.ClientID,
		"redirect_uri":  request.RedirectURI,
		"code_verifier": request.CodeVerifier,
	})
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		request.TokenURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	defer func() {
		_ = response.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse+1))
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	if len(body) > maxTokenResponse {
		return oauthpkce.TokenSet{}, errors.New("claude token response exceeds size limit")
	}
	var result tokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return oauthpkce.TokenSet{}, &oauthpkce.TokenExchangeError{
			StatusCode: response.StatusCode,
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return oauthpkce.TokenSet{}, &oauthpkce.TokenExchangeError{
			StatusCode:  response.StatusCode,
			Code:        result.Error,
			Description: result.ErrorDescription,
		}
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		return oauthpkce.TokenSet{}, errors.New("claude token response omitted credentials")
	}
	expiry := time.Time{}
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return oauthpkce.TokenSet{
		AccessToken:  result.AccessToken,
		TokenType:    result.TokenType,
		RefreshToken: result.RefreshToken,
		Scope:        result.Scope,
		Expiry:       expiry,
	}, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
