package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
)

const webSocketWriteTimeout = 30 * time.Second

func (handler *Handler) handleResponsesWebSocket(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodGet {
		writeAPIError(
			writer,
			http.StatusMethodNotAllowed,
			"WebSocket upgrade requires GET",
			"invalid_request_error",
			"method_not_allowed",
		)
		return
	}

	connection, err := websocket.Accept(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(handler.config.MaxRequestSize)

	ctx := request.Context()
	sessionID := requestSessionID(request)
	for {
		messageType, message, readErr := connection.Read(ctx)
		if readErr != nil {
			status := websocket.CloseStatus(readErr)
			if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
				return
			}
			return
		}
		if messageType != websocket.MessageText {
			if !handler.writeWebSocketError(
				ctx,
				connection,
				"invalid_request_error",
				"WebSocket events must be JSON text messages",
			) {
				return
			}
			continue
		}

		body, decodeErr := decodeResponseCreate(message)
		if decodeErr != nil {
			if !handler.writeWebSocketError(
				ctx,
				connection,
				"invalid_request_error",
				decodeErr.Error(),
			) {
				return
			}
			continue
		}
		requestedModel, _ := body["model"].(string)
		resolution, resolutionErr := handler.config.ModelCatalog.Resolve(
			requestedModel,
			handler.config.DefaultModel,
		)
		if resolutionErr != nil {
			if !handler.writeWebSocketError(
				ctx,
				connection,
				"invalid_request_error",
				resolutionErr.Error(),
			) {
				return
			}
			continue
		}
		if resolution.Provider != accountstore.ProviderCodex {
			if !handler.writeWebSocketError(
				ctx,
				connection,
				"unsupported_endpoint",
				"Claude models are not supported by the Responses WebSocket",
			) {
				return
			}
			continue
		}
		body["model"] = resolution.UpstreamModel
		normalized, normalizeErr := normalizeResponses(
			body,
			handler.config.DefaultModel,
			sessionID,
		)
		if normalizeErr != nil {
			if !handler.writeWebSocketError(
				ctx,
				connection,
				"invalid_request_error",
				normalizeErr.Error(),
			) {
				return
			}
			continue
		}

		response, upstreamErr := handler.upstream(ctx, normalized, sessionID)
		if upstreamErr != nil {
			errorType, message := webSocketProxyError(upstreamErr)
			if !handler.writeWebSocketError(
				ctx,
				connection,
				errorType,
				message,
			) {
				return
			}
			continue
		}
		ok := handler.forwardWebSocketResponse(ctx, connection, response)
		response.Body.Close()
		if !ok {
			return
		}
	}
}

func webSocketProxyError(err error) (string, string) {
	if errors.Is(err, codexpool.ErrAuthenticationRequired) ||
		errors.Is(err, codexauth.ErrReauthenticationRequired) {
		return "authentication_required",
			"Codex authentication is required; run 'rute account reauth NAME'"
	}
	if errors.Is(err, codexpool.ErrAccountsUnavailable) {
		return "upstream_accounts_unavailable",
			"All Codex accounts are temporarily unavailable"
	}
	return "upstream_error", "Codex upstream is unavailable"
}

func decodeResponseCreate(message []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.UseNumber()

	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		return nil, errors.New("invalid JSON event")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("event must contain one JSON object")
	}
	eventType, _ := event["type"].(string)
	if eventType != "response.create" {
		return nil, errors.New("only response.create events are supported")
	}
	if response, ok := event["response"].(map[string]any); ok {
		return response, nil
	}

	delete(event, "type")
	delete(event, "event_id")
	return event, nil
}

func (handler *Handler) forwardWebSocketResponse(
	ctx context.Context,
	connection *websocket.Conn,
	response *http.Response,
) bool {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstreamErr := upstreamAPIError(response)
		return handler.writeWebSocketError(
			ctx,
			connection,
			upstreamErr.Type,
			upstreamErr.Message,
		)
	}

	contentType := response.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		body, err := io.ReadAll(io.LimitReader(response.Body, defaultMaxRequestBytes+1))
		if err != nil || len(body) > defaultMaxRequestBytes || !json.Valid(body) {
			return handler.writeWebSocketError(
				ctx,
				connection,
				"upstream_error",
				"Codex upstream returned an invalid response",
			)
		}
		return writeWebSocketMessage(ctx, connection, body)
	}

	err := readSSE(ctx, response.Body, func(event sseEvent) error {
		payload := responseEventPayload(event)
		if payload == nil {
			return nil
		}
		if !writeWebSocketMessage(ctx, connection, payload) {
			return errors.New("write WebSocket event")
		}
		return nil
	})
	if err != nil {
		return handler.writeWebSocketError(
			ctx,
			connection,
			"upstream_error",
			"Codex upstream stream failed",
		)
	}
	return true
}

func (handler *Handler) writeWebSocketError(
	ctx context.Context,
	connection *websocket.Conn,
	errorType string,
	message string,
) bool {
	if errorType == "" {
		errorType = "upstream_error"
	}
	payload, err := json.Marshal(map[string]any{
		"type": "error",
		"error": apiError{
			Message: message,
			Type:    errorType,
			Code:    errorType,
		},
	})
	if err != nil {
		return false
	}
	return writeWebSocketMessage(ctx, connection, payload)
}

func writeWebSocketMessage(
	ctx context.Context,
	connection *websocket.Conn,
	payload []byte,
) bool {
	writeCtx, cancel := context.WithTimeout(ctx, webSocketWriteTimeout)
	defer cancel()
	return connection.Write(writeCtx, websocket.MessageText, payload) == nil
}
