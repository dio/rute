package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/providers/claude"
)

func (handler *Handler) handleClaudeMessages(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(
			writer,
			http.StatusMethodNotAllowed,
			"method not allowed",
			"invalid_request_error",
			"method_not_allowed",
		)
		return
	}
	body, err := handler.decodeBody(writer, request)
	if err != nil {
		writeClaudeError(writer, http.StatusBadRequest, "invalid JSON request", "invalid_request_error")
		return
	}
	requestedModel, _ := body["model"].(string)
	resolution, err := handler.config.ModelCatalog.Resolve(requestedModel, "")
	if err != nil {
		writeClaudeError(writer, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if !resolution.Mapped &&
		!strings.HasPrefix(strings.TrimSpace(requestedModel), accountstore.ProviderCodex+"/") {
		resolution.Provider = accountstore.ProviderClaude
		resolution.UpstreamModel = strings.TrimPrefix(
			strings.TrimSpace(requestedModel),
			accountstore.ProviderClaude+"/",
		)
	}
	if resolution.Provider != accountstore.ProviderClaude {
		writeClaudeError(
			writer,
			http.StatusBadRequest,
			"/v1/messages requires a Claude model",
			"invalid_request_error",
		)
		return
	}
	body["model"] = resolution.UpstreamModel
	payload, err := json.Marshal(body)
	if err != nil {
		writeClaudeError(writer, http.StatusBadRequest, "invalid request body", "invalid_request_error")
		return
	}
	response, err := handler.claudeUpstream(
		request.Context(),
		payload,
		request.Header,
		requestSessionID(request),
	)
	if err != nil {
		handler.writeClaudeProxyError(writer, err)
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()
	copyClaudeResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		_ = copyResponseStream(request.Context(), writer, response.Body)
		return
	}
	_, _ = io.Copy(writer, response.Body)
}

func (handler *Handler) claudeUpstream(
	ctx context.Context,
	payload []byte,
	downstreamHeaders http.Header,
	sessionID string,
) (*http.Response, error) {
	credentials := handler.credentials[accountstore.ProviderClaude]
	if credentials == nil {
		return nil, codexpool.ErrAuthenticationRequired
	}
	excluded := make(map[string]struct{})
	for {
		lease, err := credentials.Acquire(ctx, sessionID, excluded)
		if err != nil {
			return nil, err
		}
		response, err := handler.doClaudeUpstream(
			ctx,
			payload,
			downstreamHeaders,
			lease.Token.AccessToken,
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
			refreshed, refreshErr := credentials.ForceRefresh(ctx, lease.AccountID)
			if refreshErr != nil {
				excluded[lease.AccountID] = struct{}{}
				continue
			}
			response, err = handler.doClaudeUpstream(
				ctx,
				payload,
				downstreamHeaders,
				refreshed.Token.AccessToken,
			)
			if err != nil {
				return nil, err
			}
			if authorizationStatus(response.StatusCode) {
				discardResponse(response)
				credentials.MarkAuthorizationFailure(lease.AccountID)
				excluded[lease.AccountID] = struct{}{}
				continue
			}
			if response.StatusCode != http.StatusTooManyRequests {
				return response, nil
			}
		}
		until := rateLimitUntil(response)
		discardResponse(response)
		credentials.MarkRateLimited(lease.AccountID, until)
		excluded[lease.AccountID] = struct{}{}
	}
}

func (handler *Handler) doClaudeUpstream(
	ctx context.Context,
	payload []byte,
	downstreamHeaders http.Header,
	accessToken string,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		handler.config.ClaudeUpstreamURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
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
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json, text/event-stream")
	}
	if request.Header.Get("Anthropic-Version") == "" {
		request.Header.Set("Anthropic-Version", claude.APIVersion)
	}
	request.Header.Set(
		"Anthropic-Beta",
		mergeHeaderToken(request.Header.Get("Anthropic-Beta"), claude.OAuthCapability),
	)
	response, err := handler.config.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func mergeHeaderToken(value, required string) string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts)+1)
	found := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == required {
			found = true
		}
		result = append(result, part)
	}
	if !found {
		result = append(result, required)
	}
	return strings.Join(result, ",")
}

func copyClaudeResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		lower := strings.ToLower(key)
		if lower != "content-type" &&
			lower != "request-id" &&
			lower != "retry-after" &&
			!strings.Contains(lower, "ratelimit") {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
	destination.Set("Cache-Control", "no-store")
}

func copyResponseStream(
	ctx context.Context,
	writer http.ResponseWriter,
	reader io.Reader,
) error {
	controller := http.NewResponseController(writer)
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if _, err := writer.Write(buffer[:count]); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func writeClaudeError(writer http.ResponseWriter, status int, message, errorType string) {
	writeJSON(writer, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

func (handler *Handler) writeClaudeProxyError(writer http.ResponseWriter, err error) {
	if setRateLimitRetryAfter(writer, err) {
		writeClaudeError(writer, http.StatusTooManyRequests,
			"All Claude accounts are rate-limited; retry after the indicated cooldown",
			"rate_limit_error")
		return
	}
	switch {
	case errors.Is(err, codexpool.ErrAuthenticationRequired),
		errors.Is(err, codexauth.ErrReauthenticationRequired):
		writeClaudeError(
			writer,
			http.StatusServiceUnavailable,
			"Claude authentication is required; run 'rute account reauth -provider=claude NAME'",
			"authentication_error",
		)
	case errors.Is(err, codexpool.ErrAccountsUnavailable):
		writeClaudeError(
			writer,
			http.StatusServiceUnavailable,
			"All Claude accounts are temporarily unavailable",
			"api_error",
		)
	default:
		writeClaudeError(
			writer,
			http.StatusBadGateway,
			"Claude upstream is unavailable",
			"api_error",
		)
	}
}

// setRateLimitRetryAfter reports pool-wide rate limiting without exposing upstream bodies.
func setRateLimitRetryAfter(writer http.ResponseWriter, err error) bool {
	var unavailable *codexpool.UnavailableError
	if !errors.As(err, &unavailable) || !unavailable.RateLimited {
		return false
	}
	if !unavailable.RetryAt.IsZero() {
		seconds := max(1, int(math.Ceil(time.Until(unavailable.RetryAt).Seconds())))
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	return true
}
