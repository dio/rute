// Package codexpool coordinates persisted Codex accounts for the proxy.
package codexpool

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAuthenticationRequired means every configured account needs login.
	ErrAuthenticationRequired = errors.New("codexpool: authentication required")
	// ErrAccountsUnavailable means no account is currently eligible.
	ErrAccountsUnavailable = errors.New("codexpool: accounts unavailable")
)

// UnavailableError records when the earliest account cooldown ends.
type UnavailableError struct {
	RetryAt time.Time
}

func (err *UnavailableError) Error() string {
	if err.RetryAt.IsZero() {
		return ErrAccountsUnavailable.Error()
	}
	return fmt.Sprintf(
		"%s until %s",
		ErrAccountsUnavailable,
		err.RetryAt.UTC().Format(time.RFC3339),
	)
}

// Unwrap allows errors.Is(err, ErrAccountsUnavailable).
func (err *UnavailableError) Unwrap() error {
	return ErrAccountsUnavailable
}
