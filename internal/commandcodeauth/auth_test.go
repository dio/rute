package commandcodeauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// postCallback simulates the Studio: its page posts the result to the loopback
// callback as JSON, from the Studio origin.
func postCallback(t *testing.T, callback, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, callback, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new callback request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://commandcode.ai")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post callback: %v", err)
	}
	return response
}

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
			response := postCallback(t, callback, fmt.Sprintf(
				`{"apiKey":"key-123","state":%q,"userId":"user-1","userName":"alice","keyName":"laptop"}`,
				state,
			))
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
			response := postCallback(t, callback, fmt.Sprintf(
				`{"error":"access_denied","error_description":"denied by user","state":%q}`,
				state,
			))
			return response.Body.Close()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("error = %v", err)
	}
}

func TestCallbackPostsResultWithCORS(t *testing.T) {
	t.Parallel()
	completed := make(chan result, 1)
	handler := callbackHandler("expected-state", func(value result) { completed <- value })

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/callback",
		strings.NewReader(`{"apiKey":"key-123","state":"expected-state",`+
			`"userId":"user-1","userName":"alice","keyName":"laptop"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://commandcode.ai")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Errorf("body = %s", recorder.Body)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://commandcode.ai" {
		t.Errorf("allow-origin = %q", got)
	}
	select {
	case value := <-completed:
		if value.err != nil || value.credentials.APIKey != "key-123" {
			t.Errorf("completion = %+v", value)
		}
	default:
		t.Fatal("posted callback did not complete the login")
	}
}

func TestCallbackAnswersPreflight(t *testing.T) {
	t.Parallel()
	handler := callbackHandler("expected-state", func(result) {})
	request := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/callback", nil)
	request.Header.Set("Origin", "https://commandcode.ai")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	for header, want := range map[string]string{
		"Access-Control-Allow-Origin":          "https://commandcode.ai",
		"Access-Control-Allow-Methods":         "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers":         "Content-Type",
		"Access-Control-Allow-Private-Network": "true",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestCallbackRejectsMismatchedState(t *testing.T) {
	t.Parallel()

	// A posted result for another attempt fails the login immediately.
	completed := make(chan result, 1)
	handler := callbackHandler("expected-state", func(value result) { completed <- value })
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/callback",
		strings.NewReader(`{"apiKey":"key","state":"wrong"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
	select {
	case value := <-completed:
		if value.err == nil {
			t.Fatal("mismatched callback completed without an error")
		}
	default:
		t.Fatal("mismatched callback did not complete the login")
	}

	// A bare GET must not abort a login that is still in progress.
	bare := make(chan result, 1)
	handler = callbackHandler("expected-state", func(value result) { bare <- value })
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet, "http://127.0.0.1/callback", nil,
	))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("bare status = %d", recorder.Code)
	}
	select {
	case <-bare:
		t.Fatal("a bare request completed the login")
	default:
	}
}
