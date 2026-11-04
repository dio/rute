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
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/internal/modelcatalog"
	"github.com/dio/rute/providers/claude"
	"github.com/dio/rute/providers/codex"
	"github.com/dio/rute/providers/commandcode"
)

const (
	defaultMaxRequestBytes = 32 << 20
	maxUpstreamErrorBytes  = 1 << 20
)

// CredentialSource supplies account leases for one provider. A codexpool.Pool
// implements it.
type CredentialSource interface {
	Acquire(
		ctx context.Context,
		sessionID string,
		excluded map[string]struct{},
	) (codexpool.Lease, error)
	ForceRefresh(ctx context.Context, accountID string) (codexpool.Lease, error)
	MarkRateLimited(accountID string, until time.Time, reason string)
	MarkAuthorizationFailure(accountID string)
	Release(accountID string)
}

// Config configures the proxy handler.
type Config struct {
	UpstreamURL       string
	ClaudeUpstreamURL string
	// CodexAPIURL and ClaudeAPIURL are the API endpoints used by API-key
	// accounts for those providers.
	CodexAPIURL  string
	ClaudeAPIURL string
	// CommandCodeUpstreamURL overrides the Command Code Provider API base URL.
	// The wire endpoints are appended to it.
	CommandCodeUpstreamURL string
	DefaultModel           string
	HTTPClient             *http.Client
	MaxRequestSize         int64
	ModelCatalog           *modelcatalog.Catalog
	// Metrics, when set, receives one latency record per inference call.
	Metrics latency.Sink
	// Log, when set, receives one line per upstream rate-limit response with
	// the account name, upstream error type and message, and rate-limit
	// headers. It never receives credentials or request content.
	Log *log.Logger
}

// endpoints holds the upstream URL for each downstream protocol a provider
// serves. An empty URL means the provider does not serve that endpoint.
type endpoints struct {
	responses       string
	chatCompletions string
	messages        string
}

// Handler routes compatible requests to Codex and Claude upstreams.
type Handler struct {
	config      Config
	credentials map[string]CredentialSource
	endpoints   map[string]endpoints
}

// New validates cfg and creates a proxy handler serving Codex credentials.
func New(cfg Config, credentials CredentialSource) (*Handler, error) {
	if credentials == nil {
		return nil, errors.New("proxy: credentials are required")
	}
	return NewMulti(cfg, map[string]CredentialSource{
		accountstore.ProviderCodex: credentials,
	})
}

// NewMulti creates a proxy with a provider-keyed credential map. The Codex
// source is the default provider and must be present for unprefixed models.
func NewMulti(cfg Config, sources map[string]CredentialSource) (*Handler, error) {
	if len(sources) == 0 {
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
	if cfg.CodexAPIURL == "" {
		cfg.CodexAPIURL = codex.APIURL
	}
	if err := validateUpstreamURL(cfg.CodexAPIURL); err != nil {
		return nil, err
	}
	if cfg.ClaudeAPIURL == "" {
		cfg.ClaudeAPIURL = claude.APIURL
	}
	if err := validateUpstreamURL(cfg.ClaudeAPIURL); err != nil {
		return nil, err
	}
	commandCodeBase := strings.TrimRight(cfg.CommandCodeUpstreamURL, "/")
	if commandCodeBase == "" {
		commandCodeBase = commandcode.APIBaseURL
	}
	if err := validateUpstreamURL(commandCodeBase); err != nil {
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
	credentials := make(map[string]CredentialSource, len(sources))
	byProvider := make(map[string]endpoints, len(sources))
	for provider, source := range sources {
		if source == nil {
			continue
		}
		credentials[provider] = source
		switch provider {
		case accountstore.ProviderCodex:
			byProvider[provider] = endpoints{
				responses:       cfg.UpstreamURL,
				chatCompletions: cfg.UpstreamURL,
			}
		case accountstore.ProviderClaude:
			byProvider[provider] = endpoints{
				messages:        cfg.ClaudeUpstreamURL,
				chatCompletions: cfg.ClaudeUpstreamURL,
			}
		case accountstore.ProviderCommandCode:
			byProvider[provider] = endpoints{
				responses:       commandCodeBase + commandcode.ResponsesPath,
				chatCompletions: commandCodeBase + commandcode.ChatCompletionsPath,
				messages:        commandCodeBase + commandcode.MessagesPath,
			}
		}
	}
	if credentials[accountstore.ProviderCodex] == nil {
		return nil, errors.New("proxy: Codex credentials are required")
	}
	return &Handler{
		config:      cfg,
		credentials: credentials,
		endpoints:   byProvider,
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

// upstream sends body to Codex. The returned body releases the account lease
// when closed.
func (handler *Handler) upstream(
	ctx context.Context,
	endpoint string,
	body map[string]any,
	sessionID string,
) (*http.Response, error) {
	model, _ := body["model"].(string)
	call := handler.newCall(ctx, accountstore.ProviderCodex, endpoint, model)
	response, err := handler.codexAttempts(ctx, call, body, sessionID)
	if err != nil {
		call.fail(err)
		return nil, err
	}
	return call.wrap(response), nil
}

func (handler *Handler) codexAttempts(
	ctx context.Context,
	call *upstreamCall,
	body map[string]any,
	sessionID string,
) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("proxy: encode upstream request: %w", err)
	}
	credentials := handler.credentials[accountstore.ProviderCodex]
	excluded := make(map[string]struct{})
	for {
		lease, err := credentials.Acquire(ctx, sessionID, excluded)
		if err != nil {
			return nil, err
		}
		call.acquired(lease)
		response, err := handler.doUpstream(ctx, payload, sessionID, lease.AuthMode, lease.Token)
		if err != nil {
			return nil, err
		}
		if !authorizationStatus(response.StatusCode) &&
			response.StatusCode != http.StatusTooManyRequests {
			return response, nil
		}
		if authorizationStatus(response.StatusCode) {
			discardResponse(response)
			refreshed, refreshErr := credentials.ForceRefresh(ctx, lease.AccountID)
			if refreshErr != nil {
				excluded[lease.AccountID] = struct{}{}
				call.retry()
				continue
			}
			response, err = handler.doUpstream(ctx, payload, sessionID, refreshed.AuthMode, refreshed.Token)
			if err != nil {
				return nil, err
			}
			if authorizationStatus(response.StatusCode) {
				discardResponse(response)
				credentials.MarkAuthorizationFailure(lease.AccountID)
				excluded[lease.AccountID] = struct{}{}
				call.retry()
				continue
			}
			if response.StatusCode != http.StatusTooManyRequests {
				return response, nil
			}
		}
		until := rateLimitUntil(response)
		reason := handler.logRateLimit(accountstore.ProviderCodex, lease.AccountName, response, until)
		discardResponse(response)
		credentials.MarkRateLimited(lease.AccountID, until, reason)
		excluded[lease.AccountID] = struct{}{}
		call.retry()
	}
}

const maxLoggedMessage = 300

// logRateLimit reports why the upstream refused a call and returns a short
// reason: Claude's exhausted quota window, or the upstream error type.
// rateLimitUntil has already buffered the body, so it can be read again here.
func (handler *Handler) logRateLimit(
	provider string,
	account string,
	response *http.Response,
	until time.Time,
) string {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes))
	response.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	reason := response.Header.Get("Anthropic-Ratelimit-Unified-Representative-Claim")
	if reason == "" {
		reason = envelope.Error.Type
	}
	if handler.config.Log == nil {
		return reason
	}
	message := envelope.Error.Message
	if len(message) > maxLoggedMessage {
		message = message[:maxLoggedMessage] + "..."
	}
	var limits []string
	for key, values := range response.Header {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "ratelimit") || lower == "retry-after" {
			limits = append(limits, lower+"="+strings.Join(values, ","))
		}
	}
	slices.Sort(limits)
	handler.config.Log.Printf(
		"%s account %q rate-limited until %s: status=%d type=%q code=%q message=%q %s",
		provider,
		account,
		until.UTC().Format(time.RFC3339),
		response.StatusCode,
		envelope.Error.Type,
		envelope.Error.Code,
		message,
		strings.Join(limits, " "),
	)
	return reason
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
	authMode accountstore.AuthMode,
	token codexauth.Token,
) (*http.Response, error) {
	url := handler.config.UpstreamURL
	if authMode == accountstore.AuthModeAPIKey {
		url = handler.config.CodexAPIURL
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("proxy: create upstream request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	if authMode != accountstore.AuthModeAPIKey {
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("originator", codex.Originator)
		request.Header.Set("session_id", sessionID)
		request.Header.Set("User-Agent", codex.UserAgent)
		if token.AccountID != "" {
			request.Header.Set("ChatGPT-Account-ID", token.AccountID)
		}
	}

	response, err := handler.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("proxy: Codex upstream request: %w", err)
	}
	return response, nil
}

// commandCodeLabel names the provider in client-facing errors.
const commandCodeLabel = "Command Code"

// errUnsupportedEndpoint reports a downstream endpoint the resolved provider
// does not serve.
var errUnsupportedEndpoint = errors.New("proxy: endpoint is not supported for this provider")

// commandCodeURL returns the Command Code upstream URL for a downstream
// endpoint, or an empty string when the provider does not serve it.
func (handler *Handler) commandCodeURL(endpoint string) string {
	urls := handler.endpoints[accountstore.ProviderCommandCode]
	switch endpoint {
	case "responses", "responses_ws":
		return urls.responses
	case "chat_completions":
		return urls.chatCompletions
	case "messages":
		return urls.messages
	}
	return ""
}

// commandCodeUpstream sends payload to the Command Code endpoint that serves
// endpoint, using the selected account's API key. The returned body releases
// the account lease when closed.
func (handler *Handler) commandCodeUpstream(
	ctx context.Context,
	endpoint string,
	model string,
	payload []byte,
	downstreamHeaders http.Header,
	sessionID string,
) (*http.Response, error) {
	url := handler.commandCodeURL(endpoint)
	if url == "" {
		return nil, errUnsupportedEndpoint
	}
	if handler.credentials[accountstore.ProviderCommandCode] == nil {
		return nil, codexpool.ErrAuthenticationRequired
	}
	call := handler.newCall(ctx, accountstore.ProviderCommandCode, endpoint, model)
	response, err := handler.commandCodeAttempts(ctx, call, endpoint, url, payload, downstreamHeaders, sessionID)
	if err != nil {
		call.fail(err)
		return nil, err
	}
	return call.wrap(response), nil
}

// commandCodeUpstreamJSON encodes a JSON body and sends it upstream.
func (handler *Handler) commandCodeUpstreamJSON(
	ctx context.Context,
	endpoint string,
	model string,
	body map[string]any,
	downstreamHeaders http.Header,
	sessionID string,
) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("proxy: encode upstream request: %w", err)
	}
	return handler.commandCodeUpstream(ctx, endpoint, model, payload, downstreamHeaders, sessionID)
}

func (handler *Handler) commandCodeAttempts(
	ctx context.Context,
	call *upstreamCall,
	endpoint string,
	url string,
	payload []byte,
	downstreamHeaders http.Header,
	sessionID string,
) (*http.Response, error) {
	source := handler.credentials[accountstore.ProviderCommandCode]
	excluded := make(map[string]struct{})
	for {
		lease, err := source.Acquire(ctx, sessionID, excluded)
		if err != nil {
			return nil, err
		}
		call.acquired(lease)
		response, err := handler.doCommandCodeUpstream(
			ctx, endpoint, url, payload, downstreamHeaders, lease.Token.AccessToken,
		)
		if err != nil {
			return nil, err
		}
		if !authorizationStatus(response.StatusCode) &&
			response.StatusCode != http.StatusTooManyRequests {
			return response, nil
		}
		if authorizationStatus(response.StatusCode) {
			discardResponse(response)
			refreshed, refreshErr := source.ForceRefresh(ctx, lease.AccountID)
			if refreshErr != nil {
				excluded[lease.AccountID] = struct{}{}
				call.retry()
				continue
			}
			response, err = handler.doCommandCodeUpstream(
				ctx, endpoint, url, payload, downstreamHeaders, refreshed.Token.AccessToken,
			)
			if err != nil {
				return nil, err
			}
			if authorizationStatus(response.StatusCode) {
				discardResponse(response)
				source.MarkAuthorizationFailure(lease.AccountID)
				excluded[lease.AccountID] = struct{}{}
				call.retry()
				continue
			}
			if response.StatusCode != http.StatusTooManyRequests {
				return response, nil
			}
		}
		until := rateLimitUntil(response)
		reason := handler.logRateLimit(accountstore.ProviderCommandCode, lease.AccountName, response, until)
		discardResponse(response)
		source.MarkRateLimited(lease.AccountID, until, reason)
		excluded[lease.AccountID] = struct{}{}
		call.retry()
	}
}

func (handler *Handler) doCommandCodeUpstream(
	ctx context.Context,
	endpoint string,
	url string,
	payload []byte,
	downstreamHeaders http.Header,
	apiKey string,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("proxy: create upstream request: %w", err)
	}
	for key, values := range downstreamHeaders {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "anthropic-") &&
			!strings.HasPrefix(lower, "x-claude-code-") {
			continue
		}
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Del("Authorization")
	request.Header.Del("X-Api-Key")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json, text/event-stream")
	}
	if endpoint == "messages" && request.Header.Get("Anthropic-Version") == "" {
		request.Header.Set("Anthropic-Version", claude.APIVersion)
	}

	response, err := handler.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("proxy: Command Code upstream request: %w", err)
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

// providerDisplayLabel returns the human-facing name for a provider id.
func providerDisplayLabel(provider string) string {
	switch provider {
	case accountstore.ProviderCommandCode:
		return commandCodeLabel
	case accountstore.ProviderClaude:
		return "Claude"
	default:
		return "Codex"
	}
}

// writePassthroughResponse copies an upstream response back to the client
// without translation, streaming SSE bodies.
func (handler *Handler) writePassthroughResponse(
	writer http.ResponseWriter,
	request *http.Request,
	response *http.Response,
) {
	copyUpstreamResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		_ = copyResponseStream(request.Context(), writer, response.Body)
		return
	}
	_, _ = io.Copy(writer, response.Body)
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
	if setRateLimitRetryAfter(writer, err) {
		writeAPIError(
			writer,
			http.StatusTooManyRequests,
			"All Codex accounts are rate-limited; retry after the indicated cooldown",
			"rate_limit_error",
			"rate_limit_exceeded",
		)
		return
	}
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
