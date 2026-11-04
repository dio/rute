package admin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
)

const recentOnDashboard = 20

type dashboardView struct {
	Version    string
	Uptime     string
	Window     string
	Windows    []string
	Refresh    refreshOption
	Refreshes  []refreshOption
	History    bool
	CanRestart bool
	// Partial explains that history does not cover the whole window.
	Partial   string
	Error     string
	CSRF      string
	Providers []providerView
	Latency   []latencyView
	Recent    []recentView
}

type providerView struct {
	Name     string
	Summary  string
	Balance  string
	Accounts []accountView
}

type accountView struct {
	Provider  string
	Name      string
	Auth      string
	State     string
	Attention bool
	InFlight  int
	Calls     int
	OK        string
	Limited   string
	Failover  string
	WaitP50   string
	Rate      string
	CanReauth bool
}

func authLabel(mode accountstore.AuthMode) string {
	if mode == accountstore.AuthModeAPIKey {
		return "api key"
	}
	return "oauth"
}

type latencyView struct {
	Account  string
	InFlight string
	Calls    int
	OK       string
	Limited  string
	Failover string
	Canceled string
	Errors   string
	WaitP50  string
	WaitP90  string
	Rate     string
	MeanCall string
}

type recentView struct {
	Time     string
	Provider string
	Account  string
	Endpoint string
	Outcome  string
	Failed   bool
	Wait     string
	Duration string
	Tokens   string
}

// refreshOption is a dashboard auto-refresh choice. Seconds is zero for off.
type refreshOption struct {
	Name    string
	Seconds int
}

var refreshOptions = []refreshOption{
	{"5s", 5},
	{"15s", 15},
	{"1m", 60},
	{"off", 0},
}

func parseRefresh(value string) refreshOption {
	for _, option := range refreshOptions {
		if option.Name == value {
			return option
		}
	}
	return refreshOptions[0]
}

func (server *Server) handleDashboard(writer http.ResponseWriter, request *http.Request) {
	window, ok := parseWindow(request.URL.Query().Get("window"))
	if !ok {
		window, _ = parseWindow("")
	}
	refresh := parseRefresh(request.URL.Query().Get("refresh"))
	status := server.status()
	view := dashboardView{
		Version:    status.Version,
		Uptime:     formatDuration(time.Duration(status.UptimeSeconds) * time.Second),
		Window:     window.name,
		Refresh:    refresh,
		Refreshes:  refreshOptions,
		History:    status.History,
		CanRestart: status.CanRestart,
		CSRF:       server.csrf,
	}
	for _, item := range windows {
		view.Windows = append(view.Windows, item.name)
	}

	var accounts map[string]*callstore.Counters
	if status.History {
		counters, err := server.history(request.Context(), window)
		if err != nil {
			view.Error = "could not read call history"
		} else {
			accounts = callstore.ByAccount(counters)
			for _, item := range counters {
				row := item.Row()
				view.Latency = append(view.Latency, latencyView{
					Account:  row.Group,
					InFlight: row.Bucket,
					Failover: fmt.Sprint(item.Failovers),
					Calls:    row.Calls,
					OK:       latency.Share(row.OK, row.Calls),
					Limited:  latency.Share(row.RateLimited, row.Calls),
					Canceled: latency.Share(row.Canceled, row.Calls),
					Errors:   latency.Share(row.Errors, row.Calls),
					WaitP50:  latency.Seconds(row.WaitP50),
					WaitP90:  latency.Seconds(row.WaitP90),
					Rate:     latency.Rate(row.TokensPerSec),
					MeanCall: latency.Seconds(row.MeanDuration),
				})
			}
		}
		oldest, err := server.config.Calls.Oldest(request.Context())
		if err == nil && !oldest.IsZero() && oldest.After(time.Now().Add(-window.duration)) {
			view.Partial = "history starts " + formatClock(oldest)
		}
		for _, record := range server.config.Calls.Recent(recentOnDashboard) {
			view.Recent = append(view.Recent, recentRow(record))
		}
	}

	for _, provider := range status.Providers {
		view.Providers = append(view.Providers, providerRow(provider, accounts))
	}
	server.render(writer, "dashboard.html", view)
}

func providerRow(status codexpool.Status, accounts map[string]*callstore.Counters) providerView {
	view := providerView{Name: status.Provider}
	var active, cooling, reauth int
	for _, account := range status.Accounts {
		row := accountView{
			Provider: status.Provider,
			Name:     account.Name,
			Auth:     authLabel(account.AuthMode),
			InFlight: account.InFlight,
			OK:       "-",
			Limited:  "-",
			Failover: "-",
			WaitP50:  "-",
			Rate:     "-",
		}
		switch {
		case account.State == accountstore.StateReauthRequired:
			reauth++
			row.State = "needs login"
			row.Attention = true
			row.CanReauth = true
		case account.CooldownUntil != nil:
			cooling++
			row.State = "cooling until " + formatClock(*account.CooldownUntil)
			if account.Reason != "" {
				row.State += " (" + account.Reason + ")"
			}
			row.Attention = true
		default:
			active++
			row.State = "active"
		}
		if counters, ok := accounts[status.Provider+"/"+account.Name]; ok {
			stats := counters.Row()
			row.Calls = stats.Calls
			row.OK = latency.Share(stats.OK, stats.Calls)
			row.Limited = fmt.Sprint(stats.RateLimited)
			row.Failover = fmt.Sprint(counters.Failovers)
			row.WaitP50 = latency.Seconds(stats.WaitP50)
			row.Rate = latency.Rate(stats.TokensPerSec)
		}
		view.Accounts = append(view.Accounts, row)
	}
	parts := []string{fmt.Sprintf("%d active", active)}
	if cooling > 0 {
		parts = append(parts, fmt.Sprintf("%d cooling", cooling))
	}
	if reauth > 0 {
		parts = append(parts, fmt.Sprintf("%d need login", reauth))
	}
	view.Summary = fmt.Sprintf("%s (%s)", plural(len(status.Accounts), "account"), strings.Join(parts, ", "))

	view.Balance = string(status.Balance)
	if len(status.Order) > 0 {
		view.Balance += " (" + strings.Join(status.Order, " > ") + ")"
	}
	if status.MaxInFlight > 0 {
		view.Balance += fmt.Sprintf(", max %d in flight", status.MaxInFlight)
	}
	return view
}

func recentRow(record latency.Record) recentView {
	view := recentView{
		Time:     record.Time.Local().Format("Jan 2 15:04:05"),
		Provider: record.Provider,
		Account:  record.Account,
		Endpoint: record.Endpoint,
		Outcome:  record.Outcome,
		Failed:   record.Outcome != latency.OutcomeOK,
		Wait:     "-",
		Duration: latency.Seconds(record.DurationMS),
		Tokens:   "-",
	}
	if record.WaitMS > 0 {
		view.Wait = latency.Seconds(record.WaitMS)
	}
	if record.OutputTokens > 0 {
		view.Tokens = fmt.Sprint(record.OutputTokens)
	}
	return view
}

type newAccountView struct {
	CSRF      string
	Providers []string
	Provider  string
	Name      string
	Error     string
}

func (server *Server) handleNewAccount(writer http.ResponseWriter, request *http.Request) {
	server.render(writer, "new_account.html", newAccountView{
		CSRF:      server.csrf,
		Providers: server.providers(),
		Provider:  request.URL.Query().Get("provider"),
	})
}

// handleAccountForm starts an add or reauth login from an HTML form.
func (server *Server) handleAccountForm(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil || !server.checkCSRF(request) {
		writeError(writer, http.StatusForbidden, "invalid form token; reload the page and try again")
		return
	}
	provider := request.PostFormValue("provider")
	name := strings.TrimSpace(request.PostFormValue("name"))
	action := request.PostFormValue("action")
	if !validProvider(provider) {
		provider = ""
	}
	login, err := server.logins.begin(provider, name, action)
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		server.render(writer, "new_account.html", newAccountView{
			CSRF:      server.csrf,
			Providers: server.providers(),
			Provider:  provider,
			Name:      name,
			Error:     err.Error(),
		})
		return
	}
	http.Redirect(writer, request, "/logins/"+login.ID, http.StatusSeeOther)
}

type loginView struct {
	Login     Login
	Pending   bool
	Remaining string
}

func (server *Server) handleLoginPage(writer http.ResponseWriter, request *http.Request) {
	login, ok := server.logins.get(request.PathValue("id"))
	if !ok {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	view := loginView{Login: login, Pending: login.State == LoginPending}
	if view.Pending {
		view.Remaining = formatDuration(time.Until(login.ExpiresAt).Round(time.Second))
	}
	server.render(writer, "login.html", view)
}

func (server *Server) render(writer http.ResponseWriter, name string, data any) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := server.templates.ExecuteTemplate(writer, name, data); err != nil {
		http.Error(writer, "could not render page", http.StatusInternalServerError)
	}
}

func formatClock(at time.Time) string {
	local := at.Local()
	if local.YearDay() == time.Now().YearDay() && local.Year() == time.Now().Year() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2 15:04")
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60
	switch {
	case hours >= 24:
		return fmt.Sprintf("%dd%dh", hours/24, hours%24)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
