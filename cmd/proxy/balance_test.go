package main

import (
	"context"
	"strings"
	"testing"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/claudeauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/oauthpkce"
)

func TestBalanceOptionsApply(t *testing.T) {
	t.Parallel()
	newPool := func(t *testing.T) *codexpool.Pool {
		t.Helper()
		pool, err := codexpool.NewProvider(context.Background(), accountstore.NewMemoryStore(),
			accountstore.ProviderClaude, claudeauth.Config(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"default", "personal"} {
			if _, err := pool.Add(context.Background(), name, oauthpkce.TokenSet{
				AccessToken: "access-" + name, RefreshToken: "refresh-" + name,
			}); err != nil {
				t.Fatal(err)
			}
		}
		return pool
	}
	tests := []struct {
		name    string
		balance balanceOptions
		limit   int
		wantErr string
	}{
		{name: "spread", balance: balanceOptions{mode: "spread"}},
		{name: "priority", balance: balanceOptions{mode: "priority", order: "default, personal"}, limit: 6},
		{name: "failover", balance: balanceOptions{mode: "failover", order: "default"}},
		{name: "priority without limit", balance: balanceOptions{mode: "priority"}, wantErr: "max-in-flight"},
		{name: "order with spread", balance: balanceOptions{mode: "spread", order: "default"}, wantErr: "requires"},
		{name: "unknown mode", balance: balanceOptions{mode: "random"}, wantErr: "balance must be"},
		{name: "unknown account", balance: balanceOptions{mode: "failover", order: "team"}, wantErr: "does not exist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.balance.apply(newPool(t), accountstore.ProviderClaude, test.limit)
			if test.wantErr == "" && err != nil {
				t.Fatalf("apply: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("apply error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
