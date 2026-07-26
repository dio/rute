package oauthpkce

import (
	"context"
	"net/http"
)

// ExchangeRequest contains the values needed to exchange one authorization
// code. CodeVerifier and Code must always be treated as secrets.
type ExchangeRequest struct {
	ClientID     string
	TokenURL     string
	Code         string
	State        string
	RedirectURI  string
	CodeVerifier string
}

// String returns a redacted representation of the exchange request.
func (ExchangeRequest) String() string {
	return "oauthpkce.ExchangeRequest{credentials:<redacted>}"
}

// GoString returns a redacted Go-syntax representation of the exchange request.
func (request ExchangeRequest) GoString() string {
	return request.String()
}

// TokenExchanger customizes the authorization-code exchange for providers
// that do not use the standard form-encoded OAuth token request.
type TokenExchanger interface {
	Exchange(
		ctx context.Context,
		client *http.Client,
		request ExchangeRequest,
	) (TokenSet, error)
}
