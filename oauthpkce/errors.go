package oauthpkce

import (
	"errors"
	"fmt"
)

var (
	// ErrSessionClosed indicates that the login session was closed before it
	// produced a terminal callback.
	ErrSessionClosed = errors.New("oauthpkce: session closed")

	// ErrWaitCalled indicates that Wait has already been called for a session.
	ErrWaitCalled = errors.New("oauthpkce: Wait already called")

	// ErrMissingCode indicates a valid-state callback without a code or provider
	// error.
	ErrMissingCode = errors.New("oauthpkce: callback did not include an authorization code")
)

// AuthorizationError is an error returned by the authorization endpoint in the
// browser callback.
type AuthorizationError struct {
	Code        string
	Description string
	URI         string
}

// Error implements error without including authorization codes or credentials.
func (err *AuthorizationError) Error() string {
	return fmt.Sprintf("oauthpkce: authorization failed with code %q", err.Code)
}

// TokenExchangeError reports a rejected token request without placing the
// response body or provider description in its formatted error string.
type TokenExchangeError struct {
	StatusCode  int
	Code        string
	Description string
	URI         string
	cause       error
}

// Error implements error while keeping potentially sensitive response details
// out of ordinary logs.
func (err *TokenExchangeError) Error() string {
	switch {
	case err.Code != "" && err.StatusCode != 0:
		return fmt.Sprintf(
			"oauthpkce: token exchange failed with status %d and code %q",
			err.StatusCode,
			err.Code,
		)
	case err.Code != "":
		return fmt.Sprintf("oauthpkce: token exchange failed with code %q", err.Code)
	case err.StatusCode != 0:
		return fmt.Sprintf("oauthpkce: token exchange failed with status %d", err.StatusCode)
	default:
		return "oauthpkce: token exchange failed"
	}
}

// Unwrap exposes the underlying transport error for errors.Is and errors.As.
// Callers that inspect it must avoid logging provider response bodies.
func (err *TokenExchangeError) Unwrap() error {
	return err.cause
}
