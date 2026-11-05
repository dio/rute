package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/commandcodeauth"
)

type fakeAPIKeySession struct {
	credentials chan commandcodeauth.Credentials
}

func (session *fakeAPIKeySession) AuthorizationURL() string {
	return "https://commandcode.ai/studio/auth/cli?state=test"
}

func (session *fakeAPIKeySession) WaitAPIKey(ctx context.Context) (commandcodeauth.Credentials, error) {
	select {
	case credentials := <-session.credentials:
		return credentials, nil
	case <-ctx.Done():
		return commandcodeauth.Credentials{}, ctx.Err()
	}
}

func (session *fakeAPIKeySession) Close() error { return nil }

func TestStartSessionDispatchesCommandCode(t *testing.T) {
	t.Parallel()

	session, err := StartSession(accountstore.ProviderCommandCode)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer func() { _ = session.Close() }()
	if _, ok := session.(APIKeySession); !ok {
		t.Errorf("commandcode session type = %T, want APIKeySession", session)
	}
	if !strings.HasPrefix(session.AuthorizationURL(), "https://commandcode.ai/studio/auth/cli?") {
		t.Errorf("authorization URL = %q", session.AuthorizationURL())
	}
	if _, err := StartSession("unknown"); err == nil {
		t.Fatal("unknown provider was accepted")
	}
}

func TestAddDefaultsToDefaultAccountName(t *testing.T) {
	t.Parallel()

	pool, err := codexpool.NewProvider(
		context.Background(),
		accountstore.NewMemoryStore(),
		accountstore.ProviderCommandCode,
		codexauth.Config{},
		nil,
	)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	session := &fakeAPIKeySession{credentials: make(chan commandcodeauth.Credentials, 1)}
	server, err := New(Config{
		Version:      "test",
		Pools:        map[string]*codexpool.Pool{accountstore.ProviderCommandCode: pool},
		StartSession: func(string) (Pending, error) { return session, nil },
		LoginTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}
	do := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder
	}

	page := do(httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:20129/accounts/new?provider=commandcode", nil)).Body.String()
	if !strings.Contains(page, `value="default"`) {
		t.Errorf("the add form does not offer the default name:\n%s", page)
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader(`{"provider":"commandcode"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := do(request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("start login = %d: %s", recorder.Code, recorder.Body)
	}
	var login Login
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.Name != "default" {
		t.Errorf("login name = %q, want default", login.Name)
	}

	session.credentials <- commandcodeauth.Credentials{APIKey: "key", UserID: "user-1"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		polled := do(httptest.NewRequest(http.MethodGet,
			"http://127.0.0.1:20129/v1/logins/"+login.ID, nil))
		if err := json.Unmarshal(polled.Body.Bytes(), &login); err != nil {
			t.Fatal(err)
		}
		if login.State != LoginPending || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if login.State != LoginDone {
		t.Fatalf("finished login = %+v", login)
	}
	accounts := pool.Accounts()
	if len(accounts) != 1 || accounts[0].Name != "default" {
		t.Errorf("accounts = %+v", accounts)
	}
}

func TestDashboardAddsCommandCodeAPIKeyAccount(t *testing.T) {
	t.Parallel()

	pool, err := codexpool.NewProvider(
		context.Background(),
		accountstore.NewMemoryStore(),
		accountstore.ProviderCommandCode,
		codexauth.Config{},
		nil,
	)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	session := &fakeAPIKeySession{credentials: make(chan commandcodeauth.Credentials, 1)}
	server, err := New(Config{
		Version:      "test",
		Pools:        map[string]*codexpool.Pool{accountstore.ProviderCommandCode: pool},
		StartSession: func(string) (Pending, error) { return session, nil },
		LoginTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}
	do := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader(`{"provider":"commandcode","name":"work"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := do(request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("start login = %d: %s", recorder.Code, recorder.Body)
	}
	var login Login
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.State != LoginPending || !strings.HasPrefix(login.AuthorizationURL, "https://commandcode.ai/") {
		t.Fatalf("login = %+v", login)
	}

	session.credentials <- commandcodeauth.Credentials{
		APIKey: "secret-key", UserID: "user-1", UserName: "alice", KeyName: "laptop",
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		polled := do(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/v1/logins/"+login.ID, nil))
		if err := json.Unmarshal(polled.Body.Bytes(), &login); err != nil {
			t.Fatal(err)
		}
		if login.State != LoginPending || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if login.State != LoginDone {
		t.Fatalf("finished login = %+v", login)
	}

	accounts := pool.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].AuthMode != accountstore.AuthModeAPIKey || accounts[0].AccountID != "user-1" {
		t.Errorf("account = %+v", accounts[0])
	}

	page := do(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/", nil)).Body.String()
	if !strings.Contains(page, "api key") {
		t.Errorf("dashboard does not show the api key auth mode:\n%s", page)
	}
	if strings.Contains(page, "secret-key") {
		t.Error("dashboard leaks the API key")
	}
}
