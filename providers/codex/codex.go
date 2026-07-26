// Package codex provides the OAuth public-client profile used by Codex.
package codex

import (
	"net/url"

	"github.com/dio/rute/oauthpkce"
)

const (
	clientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	authorizationURL = "https://auth.openai.com/oauth/authorize"
	tokenURL         = "https://auth.openai.com/oauth/token"
	callbackPort     = 1455
	callbackPath     = "/auth/callback"
)

const (
	// ResponsesURL is the Codex Responses upstream used by the Codex CLI.
	ResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

	// Originator identifies requests as coming from the Codex CLI protocol.
	Originator = "codex_cli_rs"

	// UserAgent mirrors the Codex CLI user agent expected by the upstream.
	UserAgent = "codex_cli_rs/0.136.0"
)

// Config returns the Codex OAuth Authorization Code + PKCE configuration.
//
// The client identifier is public, not a client secret.
func Config() oauthpkce.Config {
	return oauthpkce.Config{
		ClientID:         clientID,
		AuthorizationURL: authorizationURL,
		TokenURL:         tokenURL,
		Scopes: []string{
			"openid",
			"profile",
			"email",
			"offline_access",
		},
		AuthorizationParams: url.Values{
			"codex_cli_simplified_flow":  {"true"},
			"id_token_add_organizations": {"true"},
			"originator":                 {Originator},
		},
		Loopback: oauthpkce.LoopbackConfig{
			RedirectHost: "localhost",
			Port:         callbackPort,
			CallbackPath: callbackPath,
		},
	}
}
