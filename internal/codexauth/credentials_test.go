package codexauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/rute/oauthpkce"
)

func TestSourceCoalescesConcurrentRefresh(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refreshes.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{
			"access_token":"new-access",
			"refresh_token":"new-refresh",
			"expires_in":3600
		}`)
	}))
	defer server.Close()

	source, err := New(Config{
		ClientID: "client",
		TokenURL: server.URL,
	}, oauthpkce.TokenSet{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		Expiry:       time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}

	const readers = 16
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			<-start
			token, err := source.Get(context.Background())
			if err != nil {
				t.Errorf("get token: %v", err)
				return
			}
			if token.AccessToken != "new-access" {
				t.Errorf("access token = %q", token.AccessToken)
			}
		}()
	}
	close(start)
	wait.Wait()

	if got := refreshes.Load(); got != 1 {
		t.Errorf("refresh requests = %d", got)
	}
}

func TestSourceCoalescesConcurrentFailedRefresh(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		time.Sleep(20 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(writer, `{"error":"invalid_grant"}`)
	}))
	defer server.Close()
	source, err := NewFromRefreshToken(Config{
		ClientID: "client",
		TokenURL: server.URL,
	}, "invalid-refresh", "account")
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	const readers = 16
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			<-start
			_, err := source.Get(context.Background())
			if !errors.Is(err, ErrReauthenticationRequired) {
				t.Errorf("get error = %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := refreshes.Load(); got != 1 {
		t.Errorf("refresh requests = %d, want 1", got)
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	t.Parallel()

	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-1"}}`,
	))
	source, err := New(Config{
		ClientID: "client",
		TokenURL: "https://example.com/token",
	}, oauthpkce.TokenSet{
		AccessToken: "access",
		IDToken:     "header." + payload + ".signature",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	token, err := source.Get(context.Background())
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if token.AccountID != "account-1" {
		t.Errorf("account ID = %q", token.AccountID)
	}
}
