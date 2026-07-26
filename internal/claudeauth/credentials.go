// Package claudeauth configures Claude OAuth credential refresh.
package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/oauthpkce"
	"github.com/dio/rute/providers/claude"
)

const (
	refreshBefore      = 4 * time.Hour
	maxRefreshResponse = 1 << 20
)

// Config returns a credential-source configuration for Claude.
func Config() codexauth.Config {
	return codexauth.Config{
		ClientID:      claude.ClientID,
		TokenURL:      claude.TokenURL,
		RefreshBefore: refreshBefore,
		Refresh:       refresh,
		Identity:      func(oauthpkce.TokenSet) string { return "" },
	}
}

func refresh(
	ctx context.Context,
	client *http.Client,
	config codexauth.Config,
	current codexauth.Token,
) (codexauth.Token, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": current.RefreshToken,
		"client_id":     config.ClientID,
	})
	if err != nil {
		return codexauth.Token{}, fmt.Errorf("claudeauth: encode refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		config.TokenURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return codexauth.Token{}, fmt.Errorf("claudeauth: create refresh request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return codexauth.Token{}, fmt.Errorf("claudeauth: refresh request: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRefreshResponse+1))
	if err != nil {
		return codexauth.Token{}, fmt.Errorf("claudeauth: read refresh response: %w", err)
	}
	if len(body) > maxRefreshResponse {
		return codexauth.Token{}, errors.New("claudeauth: refresh response exceeds size limit")
	}
	var result refreshResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return codexauth.Token{}, fmt.Errorf(
			"claudeauth: refresh response has status %d and invalid JSON",
			response.StatusCode,
		)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if permanentError(result.Error, result.ErrorDescription) {
			return codexauth.Token{}, codexauth.ErrReauthenticationRequired
		}
		return codexauth.Token{}, fmt.Errorf(
			"claudeauth: refresh rejected with status %d",
			response.StatusCode,
		)
	}
	if result.AccessToken == "" {
		return codexauth.Token{}, errors.New("claudeauth: refresh response omitted access token")
	}
	refreshToken := result.RefreshToken
	if refreshToken == "" {
		refreshToken = current.RefreshToken
	}
	expiry := time.Time{}
	if result.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	return codexauth.Token{
		AccessToken:  result.AccessToken,
		RefreshToken: refreshToken,
		Expiry:       expiry,
	}, nil
}

func permanentError(code, description string) bool {
	value := bytes.ToLower([]byte(code + " " + description))
	return bytes.Contains(value, []byte("invalid_grant")) ||
		bytes.Contains(value, []byte("refresh_token_expired")) ||
		bytes.Contains(value, []byte("refresh_token_invalidated"))
}

type refreshResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}
