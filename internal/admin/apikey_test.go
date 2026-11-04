package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/dio/rute/internal/accountstore"
)

func TestDashboardAddsAPIKeyAccountFromForm(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	page := fixture.do(t, httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:20129/accounts/new?provider=claude", nil))
	match := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page.Body.String())
	if match == nil {
		t.Fatalf("no CSRF token in the form:\n%s", page.Body)
	}
	post := func(values url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/accounts",
			strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return fixture.do(t, request)
	}
	values := url.Values{
		"provider": {"claude"}, "name": {"apikey"}, "action": {"add"},
		"api_key": {"sk-ant"}, "csrf": {match[1]},
	}
	recorder := post(values)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Fatalf("add api key = %d, location %q: %s",
			recorder.Code, recorder.Header().Get("Location"), recorder.Body)
	}
	found := false
	for _, account := range fixture.pool.Accounts() {
		if account.Name == "apikey" {
			found = true
			if account.AuthMode != accountstore.AuthModeAPIKey {
				t.Errorf("auth mode = %q", account.AuthMode)
			}
		}
	}
	if !found {
		t.Fatal("api-key account was not added")
	}

	values.Set("action", "reauth")
	values.Set("api_key", "sk-ant-rotated")
	if recorder := post(values); recorder.Code != http.StatusSeeOther {
		t.Fatalf("replace key = %d: %s", recorder.Code, recorder.Body)
	}

	values.Set("api_key", "")
	values.Set("action", "add")
	if recorder := post(values); recorder.Code != http.StatusBadRequest {
		t.Errorf("duplicate add = %d, want 400", recorder.Code)
	}
}

func TestLoginAPIAddsAPIKeyAccount(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader(`{"provider":"claude","name":"apikey","api_key":"sk-ant"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := fixture.do(t, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("start api-key login = %d: %s", recorder.Code, recorder.Body)
	}
	var login Login
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.State != LoginDone || login.Provider != "claude" || login.Name != "apikey" {
		t.Errorf("login = %+v", login)
	}
	if !strings.Contains(recorder.Body.String(), `"state": "done"`) {
		t.Errorf("body = %s", recorder.Body)
	}
}
