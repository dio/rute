package oauthpkce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestSession_SuccessfulExchange(t *testing.T) {
	t.Parallel()

	var (
		requestMu sync.Mutex
		request   url.Values
	)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("token request method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
			t.Errorf("token Content-Type = %q, want form encoded", got)
		}
		if err := req.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		requestMu.Lock()
		request = cloneValues(req.Form)
		requestMu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			writer,
			`{"access_token":"access-secret","refresh_token":"refresh-secret",`+
				`"id_token":"id-secret","token_type":"Bearer","scope":"openid offline_access","expires_in":3600}`,
		)
	}))
	defer tokenServer.Close()

	cfg := validTestConfig(tokenServer.URL)
	cfg.HTTPClient = tokenServer.Client()
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	session, err := client.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer session.Close()

	authorizationURL := mustParseURL(t, session.AuthorizationURL())
	query := authorizationURL.Query()
	state := query.Get("state")
	if state == "" {
		t.Fatal("authorization URL state is empty")
	}
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if strings.Contains(authorizationURL.RawQuery, "+") {
		t.Fatalf("authorization URL query contains '+': %s", authorizationURL.RawQuery)
	}

	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {state},
		"code":  {"authorization-code"},
	}, "")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("callback status = %d, body = %q", response.StatusCode, body)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	before := time.Now()
	tokens, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	if tokens.AccessToken != "access-secret" {
		t.Fatalf("AccessToken = %q", tokens.AccessToken)
	}
	if tokens.RefreshToken != "refresh-secret" {
		t.Fatalf("RefreshToken = %q", tokens.RefreshToken)
	}
	if tokens.IDToken != "id-secret" {
		t.Fatalf("IDToken = %q", tokens.IDToken)
	}
	if tokens.Scope != "openid offline_access" {
		t.Fatalf("Scope = %q", tokens.Scope)
	}
	if tokens.TokenType != "Bearer" {
		t.Fatalf("TokenType = %q", tokens.TokenType)
	}
	if tokens.Expiry.Before(before.Add(59*time.Minute)) || tokens.Expiry.After(before.Add(61*time.Minute)) {
		t.Fatalf("Expiry = %v, want about one hour from %v", tokens.Expiry, before)
	}

	requestMu.Lock()
	gotRequest := cloneValues(request)
	requestMu.Unlock()
	if got := gotRequest.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := gotRequest.Get("client_id"); got != cfg.ClientID {
		t.Fatalf("client_id = %q", got)
	}
	if got := gotRequest.Get("redirect_uri"); got != session.RedirectURI() {
		t.Fatalf("redirect_uri = %q, want %q", got, session.RedirectURI())
	}
	if got := gotRequest.Get("code"); got != "authorization-code" {
		t.Fatalf("code = %q", got)
	}
	verifier := gotRequest.Get("code_verifier")
	if verifier == "" {
		t.Fatal("code_verifier is empty")
	}
	if got, want := query.Get("code_challenge"), oauth2.S256ChallengeFromVerifier(verifier); got != want {
		t.Fatalf("code_challenge = %q, want %q", got, want)
	}
}

func TestSession_InvalidStateDoesNotConsumeSession(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	state := authorizationState(t, session)
	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {"wrong-state"},
		"code":  {"wrong-code"},
	}, "")
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("wrong-state callback status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	response = getCallback(t, session.RedirectURI(), url.Values{
		"state": {state},
		"code":  {"valid-code"},
	}, "")
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("valid callback status = %d, want 200", response.StatusCode)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestSession_RejectsParameterPollutionWithoutConsumingSession(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	state := authorizationState(t, session)
	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {state},
		"code":  {"first", "second"},
	}, "")
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("polluted callback status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	response = getCallback(t, session.RedirectURI(), url.Values{
		"state": {state},
		"code":  {"valid-code"},
	}, "")
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("valid callback status = %d, want 200", response.StatusCode)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestSession_AuthorizationError(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	response := getCallback(t, session.RedirectURI(), url.Values{
		"state":             {authorizationState(t, session)},
		"error":             {"access_denied"},
		"error_description": {"the user declined"},
	}, "")
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("callback status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.Wait(ctx)
	var authorizationErr *AuthorizationError
	if !errors.As(err, &authorizationErr) {
		t.Fatalf("Wait() error = %v, want AuthorizationError", err)
	}
	if authorizationErr.Code != "access_denied" || authorizationErr.Description != "the user declined" {
		t.Fatalf("AuthorizationError = %#v", authorizationErr)
	}
}

func TestSession_MissingCode(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {authorizationState(t, session)},
	}, "")
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		t.Fatalf("callback status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.Wait(ctx)
	if !errors.Is(err, ErrMissingCode) {
		t.Fatalf("Wait() error = %v, want ErrMissingCode", err)
	}
}

func TestSession_TokenExchangeErrorIsRedacted(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(
			writer,
			`{"error":"invalid_grant","error_description":"leaked authorization-code"}`,
		)
	}))
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {authorizationState(t, session)},
		"code":  {"authorization-code"},
	}, "")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", response.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.Wait(ctx)
	var exchangeErr *TokenExchangeError
	if !errors.As(err, &exchangeErr) {
		t.Fatalf("Wait() error = %T %v, want TokenExchangeError", err, err)
	}
	if exchangeErr.StatusCode != http.StatusBadRequest || exchangeErr.Code != "invalid_grant" {
		t.Fatalf("TokenExchangeError = %#v", exchangeErr)
	}
	if strings.Contains(err.Error(), "authorization-code") {
		t.Fatalf("formatted error exposes response detail: %q", err)
	}
}

func TestSecretBearingTypesFormatRedacted(t *testing.T) {
	t.Parallel()

	tokens := TokenSet{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		IDToken:      "id-secret",
	}
	formattedTokens := fmt.Sprintf("%+v %#v", tokens, tokens)
	for _, secret := range []string{"access-secret", "refresh-secret", "id-secret"} {
		if strings.Contains(formattedTokens, secret) {
			t.Fatalf("formatted TokenSet exposes %q: %s", secret, formattedTokens)
		}
	}

	session := &Session{
		state:    "state-secret",
		verifier: "verifier-secret",
	}
	formattedSession := fmt.Sprintf("%+v %#v", session, session)
	for _, secret := range []string{"state-secret", "verifier-secret"} {
		if strings.Contains(formattedSession, secret) {
			t.Fatalf("formatted Session exposes %q: %s", secret, formattedSession)
		}
	}
}

func TestAuthorizationError_FormatIsRedacted(t *testing.T) {
	t.Parallel()

	err := &AuthorizationError{
		Code:        "access_denied\nforged-log-line",
		Description: "sensitive provider detail",
	}
	formatted := err.Error()
	if strings.Contains(formatted, "\n") {
		t.Fatalf("AuthorizationError contains a raw newline: %q", formatted)
	}
	if strings.Contains(formatted, err.Description) {
		t.Fatalf("AuthorizationError exposes description: %q", formatted)
	}
}

func TestSession_CancelAndRepeatedWait(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := session.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}

	_, err = session.Wait(context.Background())
	if !errors.Is(err, ErrWaitCalled) {
		t.Fatalf("second Wait() error = %v, want ErrWaitCalled", err)
	}

	assertListenerClosed(t, session.RedirectURI())
}

func TestSession_ConcurrentInvalidCallbacks(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	const callbackCount = 8
	statuses := make(chan int, callbackCount)
	errorsChannel := make(chan error, callbackCount)
	var waitGroup sync.WaitGroup
	for range callbackCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			request, err := http.NewRequest(
				http.MethodGet,
				session.RedirectURI()+"?"+url.Values{
					"state": {"wrong-state"},
					"code":  {"wrong-code"},
				}.Encode(),
				nil,
			)
			if err != nil {
				errorsChannel <- err
				return
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errorsChannel <- err
				return
			}
			statuses <- response.StatusCode
			response.Body.Close()
		}()
	}
	waitGroup.Wait()
	close(statuses)
	close(errorsChannel)

	for err := range errorsChannel {
		t.Errorf("invalid callback request error = %v", err)
	}
	for status := range statuses {
		if status != http.StatusBadRequest {
			t.Errorf("invalid callback status = %d, want 400", status)
		}
	}

	response := getCallback(t, session.RedirectURI(), url.Values{
		"state": {authorizationState(t, session)},
		"code":  {"valid-code"},
	}, "")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want 200", response.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestSession_CloseBeforeWait(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := session.Wait(ctx)
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Wait() error = %v, want ErrSessionClosed", err)
	}
}

func TestClient_StartReportsFixedPortConflict(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	cfg := validTestConfig(tokenServer.URL)
	cfg.Loopback.Port = listener.Addr().(*net.TCPAddr).Port
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := client.Start(); err == nil {
		t.Fatal("Start() error = nil, want port conflict")
	}
}

func TestSession_RejectsUnexpectedHost(t *testing.T) {
	t.Parallel()

	tokenServer := successfulTokenServer(t)
	defer tokenServer.Close()
	session := startTestSession(t, tokenServer)
	defer session.Close()

	callbackURL := session.RedirectURI() + "?" + url.Values{
		"state": {authorizationState(t, session)},
		"code":  {"code"},
	}.Encode()
	request, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Host = "attacker.example:1234"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("callback request error = %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback status = %d, want 400", response.StatusCode)
	}
}

func TestBuildAuthorizationURL_RFC7636Challenge(t *testing.T) {
	t.Parallel()

	cfg := validTestConfig("http://127.0.0.1/token")
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	got, err := buildAuthorizationURL(
		cfg,
		"http://127.0.0.1:1234/callback",
		"state",
		verifier,
	)
	if err != nil {
		t.Fatalf("buildAuthorizationURL() error = %v", err)
	}
	query := mustParseURL(t, got).Query()
	if challenge := query.Get("code_challenge"); challenge != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("code_challenge = %q", challenge)
	}
}

func FuzzParseCallback(f *testing.F) {
	f.Add("state=expected&code=code", "expected")
	f.Add("state=wrong&code=code", "expected")
	f.Add("%zz", "expected")

	f.Fuzz(func(t *testing.T, rawQuery, expectedState string) {
		values, err := url.ParseQuery(rawQuery)
		if err != nil {
			return
		}
		_, _, _ = parseCallback(values, expectedState)
	})
}

func startTestSession(t *testing.T, tokenServer *httptest.Server) *Session {
	t.Helper()

	cfg := validTestConfig(tokenServer.URL)
	cfg.HTTPClient = tokenServer.Client()
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	session, err := client.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return session
}

func successfulTokenServer(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"access","token_type":"Bearer","expires_in":3600}`)
	}))
}

func authorizationState(t *testing.T, session *Session) string {
	t.Helper()
	return mustParseURL(t, session.AuthorizationURL()).Query().Get("state")
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", rawURL, err)
	}
	return parsed
}

func getCallback(
	t *testing.T,
	redirectURI string,
	values url.Values,
	requestHost string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, redirectURI+"?"+values.Encode(), nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if requestHost != "" {
		request.Host = requestHost
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("callback request error = %v", err)
	}
	return response
}

func assertListenerClosed(t *testing.T, redirectURI string) {
	t.Helper()

	parsed := mustParseURL(t, redirectURI)
	connection, err := net.DialTimeout("tcp4", parsed.Host, 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatalf("callback listener %s is still accepting connections", parsed.Host)
	}
}
