// Package proxy implements the unauthenticated loopback compatibility facade
// backed by provider OAuth sessions.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/modelcatalog"
	"github.com/dio/rute/providers/claude"
	"github.com/dio/rute/providers/codex"
)

const (
	defaultMaxRequestBytes = 32 << 20
	maxUpstreamErrorBytes  = 1 << 20
)

type credentialSource interface {
	Acquire(
		ctx context.Context,
		sessionID string,
		excluded map[string]struct{},
	) (codexpool.Lease, error)
	ForceRefresh(ctx context.Context, accountID string) (codexpool.Lease, error)
	MarkRateLimited(accountID string, until time.Time)
	MarkAuthorizationFailure(accountID string)
}

// Config configures an OpenAI-compatible proxy handler.
type Config struct {
	UpstreamURL       string
	ClaudeUpstreamURL string
	DefaultModel      string
	HTTPClient        *http.Client
	MaxRequestSize    int64
	ModelCatalog      *modelcatalog.Catalog
}

// Handler routes compatible requests to Codex and Claude upstreams.
type Handler struct {
	config      Config
	credentials map[string]credentialSource
}

// New validates cfg and creates a proxy handler.
func New(cfg Config, credentials credentialSource) (*Handler, error) {
	return NewMulti(cfg, credentials, nil)
}

// NewMulti creates a proxy with Codex and optional Claude credentials.
func NewMulti(
	cfg Config,
	codexCredentials credentialSource,
	claudeCredentials credentialSource,
) (*Handler, error) {
	credentials := codexCredentials
	if credentials == nil {
		return nil, errors.New("proxy: credentials are required")
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = codex.ResponsesURL
	}
	if err := validateUpstreamURL(cfg.UpstreamURL); err != nil {
		return nil, err
	}
	if cfg.ClaudeUpstreamURL == "" {
		cfg.ClaudeUpstreamURL = claude.MessagesURL
	}
	if err := validateUpstreamURL(cfg.ClaudeUpstreamURL); err != nil {
		return nil, err
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "gpt-5.6-sol"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.MaxRequestSize <= 0 {
		cfg.MaxRequestSize = defaultMaxRequestBytes
	}
	if cfg.ModelCatalog == nil {
		cfg.ModelCatalog = modelcatalog.Empty()
	}
	sources := map[string]credentialSource{
		accountstore.ProviderCodex: codexCredentials,
	}
	if claudeCredentials != nil {
		sources[accountstore.ProviderClaude] = claudeCredentials
	}
	return &Handler{
		config:      cfg,
		credentials: sources,
	}, nil
}

// ServeHTTP implements the proxy routes.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/healthz":
		handler.handleHealth(writer, request)
	case "/v1/responses":
		handler.handleResponses(writer, request)
	case "/v1/chat/completions":
		handler.handleChatCompletions(writer, request)
	case "/v1/messages":
		handler.handleClaudeMessages(writer, request)
	case "/v1/models":
		handler.handleModels(writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "route not found", "invalid_request_error", "not_found")
	}
}

func (handler *Handler) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writeAPIError(
			writer,
			http.StatusMethodNotAllowed,
			"method not allowed",
			"invalid_request_error",
			"method_not_allowed",
		)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = io.WriteString(writer, `{"status":"ok"}`)
	}
}

func (handler *Handler) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(
			writer,
			http.StatusMethodNotAllowed,
			"method not allowed",
			"invalid_request_error",
			"method_not_allowed",
		)
		return
	}
	models := handler.config.ModelCatalog.Models(handler.config.DefaultModel)
	data := make([]any, 0, len(models))
	for _, model := range models {
		item := map[string]any{
			"id":       model.ID,
			"object":   "model",
			"created":  0,
			"owned_by": model.Provider,
		}
		if model.DisplayName != "" {
			item["display_name"] = model.DisplayName
		}
		data = append(data, item)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"object": "list",
		"models": []any{},
		"data":   data,
	})
}

func (handler *Handler) upstream(
	ctx context.Context,
	body map[string]any,
	sessionID string,
) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("proxy: encode upstream request: %w", err)
	}
	excluded := make(map[string]struct{})
	for {
		lease, err := handler.credentials[accountstore.ProviderCodex].Acquire(
			ctx,
			sessionID,
			excluded,
		)
		if err != nil {
			return nil, err
		}
		response, err := handler.doUpstream(ctx, payload, sessionID, lease.Token)
		if err != nil {
			return nil, err
		}
		if !authorizationStatus(response.StatusCode) &&
			response.StatusCode != http.StatusTooManyRequests {
			return response, nil
		}
		if authorizationStatus(response.StatusCode) {
			discardResponse(response)
			refreshed, refreshErr := handler.credentials[accountstore.ProviderCodex].ForceRefresh(
				ctx,
				lease.AccountID,
			)
			if refreshErr != nil {
				excluded[lease.AccountID] = struct{}{}
				continue
			}
			response, err = handler.doUpstream(ctx, payload, sessionID, refreshed.Token)
			if err != nil {
				return nil, err
			}
			if authorizationStatus(response.StatusCode) {
				discardResponse(response)
				handler.credentials[accountstore.ProviderCodex].MarkAuthorizationFailure(lease.AccountID)
				excluded[lease.AccountID] = struct{}{}
				continue
			}
			if response.StatusCode != http.StatusTooManyRequests {
				return response, nil
			}
		}
		until := rateLimitUntil(response)
		discardResponse(response)
		handler.credentials[accountstore.ProviderCodex].MarkRateLimited(lease.AccountID, until)
		excluded[lease.AccountID] = struct{}{}
	}
}

func authorizationStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func discardResponse(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxUpstreamErrorBytes))
	response.Body.Close()
}

func rateLimitUntil(response *http.Response) time.Time {
	now := time.Now()
	if value := strings.TrimSpace(response.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return boundedCooldown(now, now.Add(time.Duration(seconds)*time.Second))
		}
		if retryAt, err := http.ParseTime(value); err == nil {
			return boundedCooldown(now, retryAt)
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes+1))
	if err == nil && len(body) <= maxUpstreamErrorBytes {
		response.Body = io.NopCloser(bytes.NewReader(body))
		if retryAt := resetTimeFromBody(body); !retryAt.IsZero() {
			return boundedCooldown(now, retryAt)
		}
	}
	return now.Add(time.Minute)
}

func boundedCooldown(now, retryAt time.Time) time.Time {
	if retryAt.Before(now) {
		return now
	}
	maximum := now.Add(24 * time.Hour)
	if retryAt.After(maximum) {
		return maximum
	}
	return retryAt
}

func resetTimeFromBody(body []byte) time.Time {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return time.Time{}
	}
	return findResetTime(value, 0)
}

func findResetTime(value any, depth int) time.Time {
	if depth >= 8 {
		return time.Time{}
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "reset_at", "resets_at", "retry_at":
				if parsed := parseResetValue(child); !parsed.IsZero() {
					return parsed
				}
			}
		}
		for _, child := range typed {
			if parsed := findResetTime(child, depth+1); !parsed.IsZero() {
				return parsed
			}
		}
	case []any:
		for _, child := range typed {
			if parsed := findResetTime(child, depth+1); !parsed.IsZero() {
				return parsed
			}
		}
	}
	return time.Time{}
}

func parseResetValue(value any) time.Time {
	switch typed := value.(type) {
	case float64:
		if typed > 1_000_000_000_000 {
			return time.UnixMilli(int64(typed))
		}
		return time.Unix(int64(typed), 0)
	case string:
		if parsed, err := time.Parse(time.RFC3339, typed); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func (handler *Handler) doUpstream(
	ctx context.Context,
	payload []byte,
	sessionID string,
	token codexauth.Token,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		handler.config.UpstreamURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("proxy: create upstream request: %w", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("originator", codex.Originator)
	request.Header.Set("session_id", sessionID)
	request.Header.Set("User-Agent", codex.UserAgent)
	if token.AccountID != "" {
		request.Header.Set("ChatGPT-Account-ID", token.AccountID)
	}

	response, err := handler.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("proxy: Codex upstream request: %w", err)
	}
	return response, nil
}

func (handler *Handler) decodeBody(
	writer http.ResponseWriter,
	request *http.Request,
) (map[string]any, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, handler.config.MaxRequestSize)
	decoder := json.NewDecoder(request.Body)
	decoder.UseNumber()

	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode JSON request: %w", err)
	}
	if body == nil {
		return nil, errors.New("JSON request must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("request contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing JSON: %w", err)
	}
	return body, nil
}

func upstreamAPIError(response *http.Response) apiError {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes+1))
	if err != nil || len(body) > maxUpstreamErrorBytes {
		return apiError{
			Message: "Codex upstream request failed",
			Type:    "upstream_error",
			Code:    "upstream_error",
		}
	}
	var envelope struct {
		Error apiError `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		if envelope.Error.Type == "" {
			envelope.Error.Type = "upstream_error"
		}
		if envelope.Error.Code == "" {
			envelope.Error.Code = "upstream_error"
		}
		return envelope.Error
	}
	return apiError{
		Message: fmt.Sprintf("Codex upstream returned status %d", response.StatusCode),
		Type:    "upstream_error",
		Code:    "upstream_error",
	}
}

func writeAPIError(writer http.ResponseWriter, status int, message, errorType, code string) {
	writeJSON(writer, status, map[string]any{
		"error": apiError{
			Message: message,
			Type:    errorType,
			Code:    code,
		},
	})
}

func writeProxyError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, codexpool.ErrAuthenticationRequired),
		errors.Is(err, codexauth.ErrReauthenticationRequired):
		writeAPIError(
			writer,
			http.StatusServiceUnavailable,
			"Codex authentication is required; run 'rute-proxy account reauth NAME'",
			"authentication_error",
			"authentication_required",
		)
	case errors.Is(err, codexpool.ErrAccountsUnavailable):
		var unavailable *codexpool.UnavailableError
		if errors.As(err, &unavailable) && !unavailable.RetryAt.IsZero() {
			seconds := int(time.Until(unavailable.RetryAt).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
		writeAPIError(
			writer,
			http.StatusServiceUnavailable,
			"All Codex accounts are temporarily unavailable",
			"upstream_error",
			"upstream_accounts_unavailable",
		)
	default:
		writeAPIError(
			writer,
			http.StatusBadGateway,
			"Codex upstream is unavailable",
			"upstream_error",
			"upstream_unavailable",
		)
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func validateUpstreamURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("proxy: parse upstream URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("proxy: upstream URL must be an absolute URL without user information or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return errors.New("proxy: upstream URL must use HTTPS or loopback HTTP")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func requestSessionID(request *http.Request) string {
	for _, key := range []string{"X-Session-ID", "Session-ID"} {
		if value := cleanSessionID(request.Header.Get(key)); value != "" {
			return value
		}
	}
	return randomSessionID()
}

func cleanSessionID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, character := range value {
		isAlpha := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		isDigit := character >= '0' && character <= '9'
		isPunctuation := strings.ContainsRune("._:-", character)
		if !isAlpha && !isDigit && !isPunctuation {
			return ""
		}
	}
	return value
}

func randomSessionID() string {
	id, err := secureSessionID()
	if err == nil {
		return id
	}
	return fmt.Sprintf("rute-%d", time.Now().UnixNano())
}
