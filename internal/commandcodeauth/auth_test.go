package commandcodeauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthenticateReturnsProvisionedCredentials(t *testing.T) {
	t.Parallel()
	var authorizationURL string
	credentials, err := Authenticate(context.Background(), Options{
		StudioBaseURL: "https://studio.example",
		Timeout:       5 * time.Second,
		OnAuthorizationURL: func(value string) {
			authorizationURL = value
		},
		OpenBrowser: func(value string) error {
			parsed, err := url.Parse(value)
			if err != nil {
				return err
			}
			callback := parsed.Query().Get("callback")
			state := parsed.Query().Get("state")
			response, err := http.Get(callback + "?apiKey=key-123&state=" +
				url.QueryEscape(state) + "&userId=user-1&userName=alice&keyName=laptop")
			if err != nil {
				return err
			}
			return response.Body.Close()
		},
	})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if credentials.APIKey != "key-123" || credentials.UserID != "user-1" ||
		credentials.UserName != "alice" || credentials.KeyName != "laptop" {
		t.Errorf("credentials = %+v", credentials)
	}
	if credentials.AuthenticatedAt.IsZero() {
		t.Error("authenticated at is zero")
	}
	if !strings.HasPrefix(authorizationURL, "https://studio.example/studio/auth/cli?") {
		t.Errorf("authorization URL = %q", authorizationURL)
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if parsed.Query().Get("mode") != "redirect" {
		t.Errorf("mode = %q", parsed.Query().Get("mode"))
	}
	if !strings.HasPrefix(parsed.Query().Get("callback"), "http://127.0.0.1:") {
		t.Errorf("callback = %q", parsed.Query().Get("callback"))
	}
}

func TestAuthenticateReportsDenial(t *testing.T) {
	t.Parallel()
	_, err := Authenticate(context.Background(), Options{
		StudioBaseURL: "https://studio.example",
		Timeout:       5 * time.Second,
		OpenBrowser: func(value string) error {
			parsed, _ := url.Parse(value)
			callback := parsed.Query().Get("callback")
			state := parsed.Query().Get("state")
			response, requestErr := http.Get(callback + "?error=access_denied&state=" +
				url.QueryEscape(state) + "&error_description=denied+by+user")
			if requestErr != nil {
				return requestErr
			}
			return response.Body.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("error = %v", err)
	}
}

func TestCallbackRejectsMismatchedState(t *testing.T) {
	t.Parallel()
	completed := make(chan result, 1)
	handler := callbackHandler("expected-state", func(value result) { completed <- value })
	request := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1/callback?apiKey=key&state=wrong",
		nil,
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
	select {
	case <-completed:
		t.Fatal("mismatched state completed the login")
	default:
	}
}
