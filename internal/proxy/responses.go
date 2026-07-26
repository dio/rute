package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (handler *Handler) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if isWebSocketUpgrade(request) {
		handler.handleResponsesWebSocket(writer, request)
		return
	}
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
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalid JSON request",
			"invalid_request_error",
			"invalid_json",
		)
		return
	}
	stream, _ := body["stream"].(bool)
	sessionID := requestSessionID(request)
	normalized, err := normalizeResponses(body, handler.config.DefaultModel, sessionID)
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			err.Error(),
			"invalid_request_error",
			"invalid_request",
		)
		return
	}

	response, err := handler.upstream(request.Context(), normalized, sessionID)
	if err != nil {
		writeProxyError(writer, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstreamErr := upstreamAPIError(response)
		writeJSON(writer, response.StatusCode, map[string]any{"error": upstreamErr})
		return
	}

	if stream {
		_ = streamSSE(request.Context(), writer, response.Body)
		return
	}

	result, err := collectResponse(request.Context(), response)
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadGateway,
			"Codex upstream stream ended without a completed response",
			"upstream_error",
			"incomplete_response",
		)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func collectResponse(ctx context.Context, response *http.Response) (map[string]any, error) {
	contentType := response.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var result map[string]any
		decoder := json.NewDecoder(io.LimitReader(response.Body, defaultMaxRequestBytes))
		decoder.UseNumber()
		if err := decoder.Decode(&result); err != nil {
			return nil, err
		}
		return result, nil
	}

	var (
		completed map[string]any
		output    []any
		failed    bool
	)
	err := readSSE(ctx, response.Body, func(event sseEvent) error {
		var payload map[string]any
		if json.Unmarshal(event.data, &payload) != nil {
			return nil
		}
		eventType, _ := payload["type"].(string)
		if eventType == "" {
			eventType = event.event
		}
		switch eventType {
		case "response.output_item.done":
			if item, ok := payload["item"].(map[string]any); ok {
				output = append(output, item)
			}
		case "response.completed", "response.done":
			if responseValue, ok := payload["response"].(map[string]any); ok {
				completed = responseValue
			}
		case "response.failed", "error":
			failed = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if failed || completed == nil {
		return nil, errors.New("upstream response did not complete")
	}
	if existing, ok := completed["output"].([]any); !ok || len(existing) == 0 {
		completed["output"] = output
	}
	return completed, nil
}

func isWebSocketUpgrade(request *http.Request) bool {
	return strings.EqualFold(request.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(request.Header, "Connection", "upgrade")
}

func headerContainsToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func responseEventPayload(event sseEvent) []byte {
	data := bytes.TrimSpace(event.data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	if json.Valid(data) {
		return append([]byte{}, data...)
	}
	return nil
}
