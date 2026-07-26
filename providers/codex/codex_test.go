package codex_test

import (
	"testing"

	"github.com/dio/rute/providers/codex"
)

func TestConfig(t *testing.T) {
	t.Parallel()

	cfg := codex.Config()
	if cfg.ClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("ClientID = %q", cfg.ClientID)
	}
	if cfg.AuthorizationURL != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("AuthorizationURL = %q", cfg.AuthorizationURL)
	}
	if cfg.TokenURL != "https://auth.openai.com/oauth/token" {
		t.Fatalf("TokenURL = %q", cfg.TokenURL)
	}
	if cfg.Loopback.RedirectHost != "localhost" {
		t.Fatalf("RedirectHost = %q", cfg.Loopback.RedirectHost)
	}
	if cfg.Loopback.Port != 1455 {
		t.Fatalf("Port = %d", cfg.Loopback.Port)
	}
	if cfg.Loopback.CallbackPath != "/auth/callback" {
		t.Fatalf("CallbackPath = %q", cfg.Loopback.CallbackPath)
	}
	if cfg.AuthorizationParams.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q", cfg.AuthorizationParams.Get("originator"))
	}
}
