package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardClearsAllAccounts(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)

	page := fixture.do(t, httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:20129/accounts/clear", nil))
	body := page.Body.String()
	if !strings.Contains(body, "remove all 2 accounts") {
		t.Fatalf("clear page does not show the count:\n%s", body)
	}
	match := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if match == nil {
		t.Fatal("no CSRF token in the clear form")
	}

	post := func(values url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/accounts/clear",
			strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return fixture.do(t, request)
	}
	if got := post(url.Values{}).Code; got != http.StatusForbidden {
		t.Errorf("form without token = %d, want 403", got)
	}
	recorder := post(url.Values{"csrf": {match[1]}})
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Fatalf("clear form = %d, location %q: %s",
			recorder.Code, recorder.Header().Get("Location"), recorder.Body)
	}
	if accounts := fixture.pool.Accounts(); len(accounts) != 0 {
		t.Errorf("accounts remain: %+v", accounts)
	}
	dashboard := fixture.do(t, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20129/", nil)).Body.String()
	if strings.Contains(dashboard, "remove all accounts") {
		t.Error("the clear link is still shown with no accounts")
	}
}

func TestClearAccountsAPI(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	post := func(contentType, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20129/v1/accounts/clear",
			strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		return fixture.do(t, request)
	}
	if got := post("text/plain", `{}`).Code; got != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON body = %d, want 415", got)
	}
	if got := post("application/json", `{"provider":"nope"}`).Code; got != http.StatusBadRequest {
		t.Errorf("unknown provider = %d, want 400", got)
	}
	recorder := post("application/json", `{"provider":"claude"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed": 2`) {
		t.Fatalf("clear provider = %d: %s", recorder.Code, recorder.Body)
	}
	if accounts := fixture.pool.Accounts(); len(accounts) != 0 {
		t.Errorf("accounts remain: %+v", accounts)
	}
	recorder = post("application/json", `{}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed": 0`) {
		t.Errorf("second clear = %d: %s", recorder.Code, recorder.Body)
	}
}
