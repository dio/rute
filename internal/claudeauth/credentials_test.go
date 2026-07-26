package claudeauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/oauthpkce"
)

func TestRefreshUsesJSONAndRotatesToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["refresh_token"] != "old-refresh" ||
			body["client_id"] == "" {
			t.Errorf("request = %+v", body)
		}
		_, _ = writer.Write([]byte(
			`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`,
		))
	}))
	defer server.Close()

	config := Config()
	config.TokenURL = server.URL
	config.HTTPClient = server.Client()
	source, err := codexauth.New(config, oauthpkce.TokenSet{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	token, err := source.ForceRefresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" {
		t.Errorf("token = %v", token)
	}
}

func TestInvalidGrantRequiresReauthentication(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	config := Config()
	config.TokenURL = server.URL
	config.HTTPClient = server.Client()
	source, err := codexauth.New(config, oauthpkce.TokenSet{
		AccessToken:  "access",
		RefreshToken: "refresh",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	if _, err := source.ForceRefresh(context.Background()); err != codexauth.ErrReauthenticationRequired {
		t.Fatalf("refresh error = %v", err)
	}
}
