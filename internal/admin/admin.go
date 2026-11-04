// Package admin serves rute's loopback management interface: an HTML
// dashboard and a JSON API for account status, call history, and account
// logins. It never exposes credentials.
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/idle"
	"github.com/dio/rute/internal/pricing"
	"github.com/dio/rute/internal/routingfile"
)

const (
	defaultLoginTimeout = 5 * time.Minute
	maxRecent           = 1000
	maxFormBytes        = 16 << 10
)

// Windows are the call-history ranges the dashboard and API accept.
var windows = []struct {
	name     string
	duration time.Duration
}{
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

//go:embed templates/*.html
var templateFiles embed.FS

// Config configures a Server.
type Config struct {
	Version string
	Started time.Time
	// Pools maps provider IDs to their account pools.
	Pools map[string]*codexpool.Pool
	// Calls, when set, provides call history.
	Calls *callstore.Store
	// Prices, when set, estimates usage costs.
	Prices *pricing.File
	// Routing, when set, saves routing changes so they survive restarts.
	Routing *routingfile.File
	// ProxyURL is the base URL of the proxy listener, for clients.
	ProxyURL string
	// Activity, when set, counts attached clients as activity.
	Activity *idle.Tracker
	// IdleTimeout is how long the server stays up without activity; zero
	// means it stays up.
	IdleTimeout time.Duration
	// StartSession begins account logins. It defaults to StartOAuthSession.
	StartSession SessionStarter
	LoginTimeout time.Duration
}

// Server is the management HTTP handler.
type Server struct {
	config    Config
	logins    *logins
	csrf      string
	templates *template.Template
	mux       *http.ServeMux
}

// New creates a management server.
func New(config Config) (*Server, error) {
	if len(config.Pools) == 0 {
		return nil, errors.New("admin: at least one pool is required")
	}
	if config.StartSession == nil {
		config.StartSession = StartOAuthSession
	}
	if config.LoginTimeout <= 0 {
		config.LoginTimeout = defaultLoginTimeout
	}
	if config.Started.IsZero() {
		config.Started = time.Now()
	}
	templates, err := template.New("").Funcs(template.FuncMap{
		"join": strings.Join,
	}).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("admin: parse templates: %w", err)
	}
	server := &Server{
		config: config,
		logins: &logins{
			pools:   config.Pools,
			start:   config.StartSession,
			timeout: config.LoginTimeout,
			items:   make(map[string]*Login),
		},
		csrf:      randomID(),
		templates: templates,
		mux:       http.NewServeMux(),
	}
	server.mux.HandleFunc("GET /{$}", server.handleDashboard)
	server.mux.HandleFunc("GET /accounts/new", server.handleNewAccount)
	server.mux.HandleFunc("GET /usage", server.handleUsagePage)
	server.mux.HandleFunc("POST /accounts", server.handleAccountForm)
	server.mux.HandleFunc("GET /logins/{id}", server.handleLoginPage)
	server.mux.HandleFunc("GET /providers/{provider}/routing", server.handleRoutingPage)
	server.mux.HandleFunc("POST /providers/{provider}/routing", server.handleRoutingForm)
	server.mux.HandleFunc("GET /v1/status", server.handleStatus)
	server.mux.HandleFunc("GET /v1/attach", server.handleAttach)
	server.mux.HandleFunc("GET /v1/calls/summary", server.handleSummary)
	server.mux.HandleFunc("GET /v1/calls/recent", server.handleRecent)
	server.mux.HandleFunc("GET /v1/usage", server.handleUsageAPI)
	server.mux.HandleFunc("POST /v1/logins", server.handleLoginAPI)
	server.mux.HandleFunc("GET /v1/logins/{id}", server.handleLoginStatus)
	server.mux.HandleFunc("PUT /v1/providers/{provider}/routing", server.handleRoutingAPI)
	return server, nil
}

// ServeHTTP rejects requests whose Host is not a loopback name, which blocks
// DNS-rebinding pages from reaching the server, then routes the request.
func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !loopbackHost(request.Host) {
		writeError(writer, http.StatusForbidden, "management requests must use a loopback host")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	server.mux.ServeHTTP(writer, request)
}

func loopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// StatusResponse is the body of GET /v1/status.
type StatusResponse struct {
	Version       string    `json:"version"`
	Started       time.Time `json:"started"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	History       bool      `json:"history"`
	ProxyURL      string    `json:"proxy_url,omitempty"`
	// IdleTimeoutSeconds is zero when the server does not stop when idle.
	IdleTimeoutSeconds int64              `json:"idle_timeout_seconds"`
	Providers          []codexpool.Status `json:"providers"`
}

func (server *Server) status() StatusResponse {
	response := StatusResponse{
		Version:            server.config.Version,
		Started:            server.config.Started.UTC(),
		UptimeSeconds:      int64(time.Since(server.config.Started).Seconds()),
		History:            server.config.Calls != nil,
		ProxyURL:           server.config.ProxyURL,
		IdleTimeoutSeconds: int64(server.config.IdleTimeout.Seconds()),
	}
	for _, provider := range server.providers() {
		response.Providers = append(response.Providers, server.config.Pools[provider].Status())
	}
	return response
}

func (server *Server) providers() []string {
	names := make([]string, 0, len(server.config.Pools))
	for name := range server.config.Pools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (server *Server) handleStatus(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, server.status())
}

// handleAttach keeps the server from stopping when idle for as long as the
// request stays open. Launchers hold it while their client runs.
func (server *Server) handleAttach(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("attached\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if server.config.Activity == nil {
		<-request.Context().Done()
		return
	}
	end := server.config.Activity.Begin()
	defer end()
	select {
	case <-request.Context().Done():
	case <-server.config.Activity.Stopping():
	}
}

// SummaryRow is one row of GET /v1/calls/summary. Timing fields are null when
// no call in the row produced them.
type SummaryRow struct {
	Provider     string   `json:"provider"`
	Account      string   `json:"account"`
	InFlight     string   `json:"in_flight"`
	Calls        int      `json:"calls"`
	OK           int      `json:"ok"`
	RateLimited  int      `json:"rate_limited"`
	Canceled     int      `json:"canceled"`
	Errors       int      `json:"errors"`
	Failovers    int64    `json:"failovers"`
	OutputTokens int64    `json:"output_tokens"`
	WaitP50MS    *float64 `json:"wait_p50_ms"`
	WaitP90MS    *float64 `json:"wait_p90_ms"`
	TokensPerSec *float64 `json:"tokens_per_second_p50"`
	MeanCallMS   *float64 `json:"mean_call_ms"`
}

func (server *Server) handleSummary(writer http.ResponseWriter, request *http.Request) {
	window, ok := parseWindow(request.URL.Query().Get("window"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "window must be 15m, 1h, 24h, or 7d")
		return
	}
	counters, err := server.history(request.Context(), window)
	if err != nil {
		writeHistoryError(writer, err)
		return
	}
	response := make([]SummaryRow, 0, len(counters))
	for _, item := range counters {
		row := item.Row()
		response = append(response, SummaryRow{
			Provider:     item.Provider,
			Account:      item.Account,
			InFlight:     item.InFlight,
			Failovers:    item.Failovers,
			OutputTokens: item.OutputTokens,
			Calls:        row.Calls,
			OK:           row.OK,
			RateLimited:  row.RateLimited,
			Canceled:     row.Canceled,
			Errors:       row.Errors,
			WaitP50MS:    finite(row.WaitP50),
			WaitP90MS:    finite(row.WaitP90),
			TokensPerSec: finite(row.TokensPerSec),
			MeanCallMS:   finite(row.MeanDuration),
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"window": window.name, "rows": response})
}

func (server *Server) handleRecent(writer http.ResponseWriter, request *http.Request) {
	limit := 50
	if value := request.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > maxRecent {
			writeError(writer, http.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d", maxRecent))
			return
		}
		limit = parsed
	}
	if server.config.Calls == nil {
		writeHistoryError(writer, errHistoryDisabled)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"calls": server.config.Calls.Recent(limit)})
}

type loginRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Action   string `json:"action"`
}

// handleLoginAPI starts a login. Requiring a JSON body means a cross-site
// HTML form cannot submit it.
func (server *Server) handleLoginAPI(writer http.ResponseWriter, request *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var body loginRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxFormBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if body.Action == "" {
		body.Action = ActionAdd
	}
	login, err := server.logins.begin(body.Provider, strings.TrimSpace(body.Name), body.Action)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLoginInProgress) {
			status = http.StatusConflict
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, login)
}

func (server *Server) handleLoginStatus(writer http.ResponseWriter, request *http.Request) {
	login, ok := server.logins.get(request.PathValue("id"))
	if !ok {
		writeError(writer, http.StatusNotFound, "login not found")
		return
	}
	writeJSON(writer, http.StatusOK, login)
}

var errHistoryDisabled = errors.New("call history is disabled; start rute without -metrics-db=off")

func (server *Server) history(ctx context.Context, window windowRange) ([]*callstore.Counters, error) {
	if server.config.Calls == nil {
		return nil, errHistoryDisabled
	}
	return server.config.Calls.Summary(ctx, time.Now().Add(-window.duration))
}

func writeHistoryError(writer http.ResponseWriter, err error) {
	if errors.Is(err, errHistoryDisabled) {
		writeError(writer, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeError(writer, http.StatusInternalServerError, "could not read call history")
}

type windowRange struct {
	name     string
	duration time.Duration
}

func parseWindow(value string) (windowRange, bool) {
	if value == "" {
		value = "1h"
	}
	for _, window := range windows {
		if window.name == value {
			return windowRange{window.name, window.duration}, true
		}
	}
	return windowRange{}, false
}

func finite(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"message": message}})
}

func validProvider(provider string) bool {
	return provider == accountstore.ProviderCodex || provider == accountstore.ProviderClaude
}

func (server *Server) checkCSRF(request *http.Request) bool {
	token := request.PostFormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(token), []byte(server.csrf)) == 1
}
