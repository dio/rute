package oauthpkce

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNew_ValidatesConfig(t *testing.T) {
	t.Parallel()

	valid := validTestConfig("http://127.0.0.1/token")
	tests := []struct {
		name   string
		mutate func(*Config)
		match  string
	}{
		{
			name: "missing client ID",
			mutate: func(cfg *Config) {
				cfg.ClientID = ""
			},
			match: "client ID is required",
		},
		{
			name: "insecure remote authorization endpoint",
			mutate: func(cfg *Config) {
				cfg.AuthorizationURL = "http://example.com/authorize"
			},
			match: "insecure non-loopback HTTP",
		},
		{
			name: "endpoint user information",
			mutate: func(cfg *Config) {
				cfg.TokenURL = "https://user@example.com/token"
			},
			match: "must not contain user information",
		},
		{
			name: "missing scopes",
			mutate: func(cfg *Config) {
				cfg.Scopes = []string{}
			},
			match: "at least one scope",
		},
		{
			name: "scope containing whitespace",
			mutate: func(cfg *Config) {
				cfg.Scopes = []string{"openid profile"}
			},
			match: "invalid scope",
		},
		{
			name: "reserved authorization parameter",
			mutate: func(cfg *Config) {
				cfg.AuthorizationParams = url.Values{"state": {"override"}}
			},
			match: "parameter \"state\" is reserved",
		},
		{
			name: "non-loopback redirect host",
			mutate: func(cfg *Config) {
				cfg.Loopback.RedirectHost = "example.com"
			},
			match: "not an allowed IPv4 loopback host",
		},
		{
			name: "invalid callback port",
			mutate: func(cfg *Config) {
				cfg.Loopback.Port = 65536
			},
			match: "outside 0-65535",
		},
		{
			name: "callback path with query",
			mutate: func(cfg *Config) {
				cfg.Loopback.CallbackPath = "/callback?next=/"
			},
			match: "callback path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			cfg.Scopes = append([]string{}, valid.Scopes...)
			cfg.AuthorizationParams = cloneValues(valid.AuthorizationParams)
			test.mutate(&cfg)

			_, err := New(cfg)
			if err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), test.match) {
				t.Fatalf("New() error = %q, want substring %q", err, test.match)
			}
		})
	}
}

func TestNew_CopiesMutableConfiguration(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig("http://127.0.0.1/token")
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cfg.Scopes[0] = "changed"
	cfg.AuthorizationParams.Set("prompt", "changed")

	if got := client.config.Scopes[0]; got != "openid" {
		t.Fatalf("copied scope = %q, want openid", got)
	}
	if got := client.config.AuthorizationParams.Get("prompt"); got != "consent" {
		t.Fatalf("copied prompt = %q, want consent", got)
	}
}

func validTestConfig(tokenURL string) Config {
	return Config{
		ClientID:         "test-client",
		AuthorizationURL: "https://auth.example.test/authorize",
		TokenURL:         tokenURL,
		Scopes:           []string{"openid", "offline_access"},
		AuthorizationParams: url.Values{
			"prompt": {"consent"},
		},
		Loopback: LoopbackConfig{
			RedirectHost: "127.0.0.1",
			CallbackPath: "/callback",
		},
		HTTPClient: &http.Client{},
	}
}
