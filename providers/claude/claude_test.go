package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dio/rute/oauthpkce"
)

func TestJSONExchanger(t *testing.T) {
	t.Parallel()
	var received map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(
			`{"access_token":"access","refresh_token":"refresh","expires_in":3600}`,
		))
	}))
	defer server.Close()

	tokens, err := (jsonExchanger{}).Exchange(
		context.Background(),
		server.Client(),
		exchangeRequest(server.URL, "code#state"),
	)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" {
		t.Fatalf("tokens = %v", tokens)
	}
	if received["state"] != "state" ||
		received["code_verifier"] != "verifier" ||
		received["redirect_uri"] != "http://localhost/callback" {
		t.Errorf("request = %+v", received)
	}
}

func TestJSONExchangerRejectsEmbeddedStateMismatch(t *testing.T) {
	t.Parallel()
	_, err := (jsonExchanger{}).Exchange(
		context.Background(),
		http.DefaultClient,
		exchangeRequest("http://127.0.0.1:1", "code#wrong"),
	)
	if err == nil {
		t.Fatal("exchange succeeded")
	}
}

func exchangeRequest(tokenURL, code string) oauthpkce.ExchangeRequest {
	return oauthpkce.ExchangeRequest{
		ClientID:     ClientID,
		TokenURL:     tokenURL,
		Code:         code,
		State:        "state",
		RedirectURI:  "http://localhost/callback",
		CodeVerifier: "verifier",
	}
}
