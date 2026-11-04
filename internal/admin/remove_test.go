package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardRemovesAccounts(t *testing.T) {
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

	// The form removes an account and returns to the dashboard.
	values := url.Values{
		"provider": {"claude"}, "name": {"personal"}, "action": {"remove"},
		"csrf": {match[1]},
	}
	recorder := post(values)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Fatalf("remove form = %d, location %q: %s",
			recorder.Code, recorder.Header().Get("Location"), recorder.Body)
	}
	if names := accountNames(t, fixture); strings.Contains(strings.Join(names, ","), "personal") {
		t.Errorf("accounts after remove = %v", names)
	}

	// The JSON API removes an account too.
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader(`{"provider":"claude","name":"default","action":"remove"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = fixture.do(t, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed": true`) {
		t.Fatalf("remove API = %d: %s", recorder.Code, recorder.Body)
	}
	if names := accountNames(t, fixture); len(names) != 0 {
		t.Errorf("accounts after API remove = %v", names)
	}

	// Removing an account that does not exist is rejected.
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/logins",
		strings.NewReader(`{"provider":"claude","name":"missing","action":"remove"}`))
	request.Header.Set("Content-Type", "application/json")
	if recorder := fixture.do(t, request); recorder.Code != http.StatusBadRequest {
		t.Errorf("remove missing = %d, want 400", recorder.Code)
	}

	dashboard := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/", nil)).Body.String()
	if strings.Contains(dashboard, "claude/personal") {
		t.Error("dashboard still lists a removed account")
	}
}

func accountNames(t *testing.T, fixture *fixture) []string {
	t.Helper()
	var names []string
	for _, account := range fixture.pool.Accounts() {
		names = append(names, account.Name)
	}
	return names
}
