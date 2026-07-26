package oauthpkce

import (
	"fmt"
	"time"

	"golang.org/x/oauth2"
)

// TokenSet contains the credentials returned by an authorization-code
// exchange. Every token field must be treated as a secret.
type TokenSet struct {
	AccessToken  string
	TokenType    string
	RefreshToken string
	IDToken      string
	Scope        string
	Expiry       time.Time
}

// String returns a redacted representation of the token set.
func (tokens TokenSet) String() string {
	return fmt.Sprintf(
		"oauthpkce.TokenSet{TokenType:%q Scope:%q Expiry:%q credentials:<redacted>}",
		tokens.TokenType,
		tokens.Scope,
		tokens.Expiry,
	)
}

// GoString returns a redacted Go-syntax representation of the token set.
func (tokens TokenSet) GoString() string {
	return tokens.String()
}

func tokenSetFromOAuth2(token *oauth2.Token) TokenSet {
	return TokenSet{
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		RefreshToken: token.RefreshToken,
		IDToken:      extraString(token, "id_token"),
		Scope:        extraString(token, "scope"),
		Expiry:       token.Expiry,
	}
}

func extraString(token *oauth2.Token, key string) string {
	value := token.Extra(key)
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}
