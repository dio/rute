package oauthpkce

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

const (
	defaultCallbackPath = "/callback"
	defaultRedirectHost = "127.0.0.1"
)

var reservedAuthorizationParams = map[string]struct{}{
	"client_id":             {},
	"code_challenge":        {},
	"code_challenge_method": {},
	"redirect_uri":          {},
	"response_type":         {},
	"scope":                 {},
	"state":                 {},
}

// LoopbackConfig describes the redirect URI exposed by a login session.
//
// Port zero asks the operating system to choose an available port. RedirectHost
// must be localhost or 127.0.0.1; the actual listener always binds 127.0.0.1.
type LoopbackConfig struct {
	RedirectHost string
	Port         int
	CallbackPath string
}

// Config describes a public OAuth client using Authorization Code with PKCE.
type Config struct {
	ClientID            string
	AuthorizationURL    string
	TokenURL            string
	Scopes              []string
	AuthorizationParams url.Values
	Loopback            LoopbackConfig
	HTTPClient          *http.Client
	TokenExchanger      TokenExchanger
}

func normalizeConfig(cfg Config) (Config, error) {
	cfg.Scopes = append([]string{}, cfg.Scopes...)
	cfg.AuthorizationParams = cloneValues(cfg.AuthorizationParams)

	if cfg.Loopback.RedirectHost == "" {
		cfg.Loopback.RedirectHost = defaultRedirectHost
	}
	if cfg.Loopback.CallbackPath == "" {
		cfg.Loopback.CallbackPath = defaultCallbackPath
	}

	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func validateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return errors.New("oauthpkce: client ID is required")
	}
	if containsControl(cfg.ClientID) {
		return errors.New("oauthpkce: client ID contains control characters")
	}
	if _, err := validateEndpoint("authorization", cfg.AuthorizationURL); err != nil {
		return err
	}
	if _, err := validateEndpoint("token", cfg.TokenURL); err != nil {
		return err
	}
	if len(cfg.Scopes) == 0 {
		return errors.New("oauthpkce: at least one scope is required")
	}
	for _, scope := range cfg.Scopes {
		if scope == "" || strings.IndexFunc(scope, unicode.IsSpace) >= 0 || containsControl(scope) {
			return fmt.Errorf("oauthpkce: invalid scope %q", scope)
		}
	}
	for key := range cfg.AuthorizationParams {
		if key == "" || containsControl(key) {
			return fmt.Errorf("oauthpkce: invalid authorization parameter name %q", key)
		}
		if _, reserved := reservedAuthorizationParams[key]; reserved {
			return fmt.Errorf("oauthpkce: authorization parameter %q is reserved", key)
		}
	}
	if err := validateLoopback(cfg.Loopback); err != nil {
		return err
	}

	return nil
}

func validateEndpoint(name, rawURL string) (*url.URL, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("oauthpkce: parse %s endpoint: %w", name, err)
	}
	if !endpoint.IsAbs() || endpoint.Hostname() == "" {
		return nil, fmt.Errorf("oauthpkce: %s endpoint must be absolute", name)
	}
	if endpoint.User != nil {
		return nil, fmt.Errorf("oauthpkce: %s endpoint must not contain user information", name)
	}
	if endpoint.Fragment != "" {
		return nil, fmt.Errorf("oauthpkce: %s endpoint must not contain a fragment", name)
	}

	switch strings.ToLower(endpoint.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(endpoint.Hostname()) {
			return nil, fmt.Errorf("oauthpkce: %s endpoint uses insecure non-loopback HTTP", name)
		}
	default:
		return nil, fmt.Errorf("oauthpkce: %s endpoint must use HTTP or HTTPS", name)
	}

	return endpoint, nil
}

func validateLoopback(cfg LoopbackConfig) error {
	if cfg.RedirectHost != "localhost" && cfg.RedirectHost != "127.0.0.1" {
		return fmt.Errorf("oauthpkce: redirect host %q is not an allowed IPv4 loopback host", cfg.RedirectHost)
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("oauthpkce: callback port %d is outside 0-65535", cfg.Port)
	}
	if !strings.HasPrefix(cfg.CallbackPath, "/") || strings.HasPrefix(cfg.CallbackPath, "//") {
		return fmt.Errorf("oauthpkce: callback path %q must be an absolute path", cfg.CallbackPath)
	}
	parsed, err := url.ParseRequestURI(cfg.CallbackPath)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("oauthpkce: callback path %q is invalid", cfg.CallbackPath)
	}

	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, entries := range values {
		cloned[key] = append([]string{}, entries...)
	}
	return cloned
}
