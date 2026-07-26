package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type chatState struct {
	id        string
	model     string
	created   int64
	content   strings.Builder
	toolCalls []*chatToolCall
	usage     map[string]any
	completed bool
	failed    bool
}

type chatToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func (handler *Handler) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
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
	responsesBody, model, err := chatToResponses(body, handler.config.DefaultModel, sessionID)
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

	response, err := handler.upstream(request.Context(), responsesBody, sessionID)
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

	state := newChatState(model)
	if stream {
		handler.streamChat(writer, request, response.Body, state)
		return
	}
	if err := collectChat(request.Context(), response.Body, state, nil); err != nil || !state.completed || state.failed {
		writeAPIError(
			writer,
			http.StatusBadGateway,
			"Codex upstream stream ended without a completed response",
			"upstream_error",
			"incomplete_response",
		)
		return
	}
	writeJSON(writer, http.StatusOK, state.completion())
}

func chatToResponses(
	body map[string]any,
	defaultModel string,
	sessionID string,
) (map[string]any, string, error) {
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}

	if _, hasInput := body["input"]; hasInput {
		normalized, err := normalizeResponses(body, defaultModel, sessionID)
		return normalized, model, err
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok {
		return nil, "", errors.New("messages must be an array")
	}

	responsesBody := map[string]any{
		"model": model,
		"input": []any{},
	}
	input := make([]any, 0, len(rawMessages))
	hasInstructions := false
	for _, rawMessage := range rawMessages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, "", errors.New("each message must be an object")
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if !hasInstructions {
				responsesBody["instructions"] = textContent(message["content"])
				hasInstructions = true
			}
		case "user", "assistant":
			if role == "assistant" {
				if reasoning := chatReasoningItem(message); reasoning != nil {
					input = append(input, reasoning)
				}
			}
			content := chatMessageContent(role, message["content"])
			if len(content) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    role,
					"content": content,
				})
			}
			if role == "assistant" {
				input = append(input, chatToolCalls(message["tool_calls"])...)
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				return nil, "", errors.New("tool message requires tool_call_id")
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": clampCallID(callID),
				"output":  textContent(message["content"]),
			})
		default:
			return nil, "", fmt.Errorf("unsupported message role %q", role)
		}
	}
	responsesBody["input"] = input

	for _, field := range []string{"tools", "tool_choice", "reasoning", "service_tier", "text"} {
		if value, exists := body[field]; exists {
			responsesBody[field] = value
		}
	}
	if effort, ok := body["reasoning_effort"].(string); ok {
		responsesBody["reasoning"] = map[string]any{
			"effort":  effort,
			"summary": "auto",
		}
	}

	normalized, err := normalizeResponses(responsesBody, defaultModel, sessionID)
	return normalized, model, err
}

func chatMessageContent(role string, value any) []any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return []any{}
		}
		return []any{map[string]any{"type": contentType, "text": typed}}
	case []any:
		content := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				content = append(content, map[string]any{"type": contentType, "text": text})
			case "image_url":
				if role != "user" {
					continue
				}
				imageURL, detail := chatImageURL(part["image_url"])
				if imageURL != "" {
					content = append(content, map[string]any{
						"type":      "input_image",
						"image_url": imageURL,
						"detail":    detail,
					})
				}
			case "input_image":
				if role == "user" {
					content = append(content, part)
				}
			}
		}
		return content
	default:
		return []any{}
	}
}

func chatImageURL(value any) (string, string) {
	if rawURL, ok := value.(string); ok {
		return rawURL, "auto"
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", ""
	}
	rawURL, _ := object["url"].(string)
	detail, _ := object["detail"].(string)
	if detail == "" {
		detail = "auto"
	}
	return rawURL, detail
}

func chatToolCalls(value any) []any {
	rawCalls, ok := value.([]any)
	if !ok {
		return []any{}
	}
	calls := make([]any, 0, len(rawCalls))
	for _, rawCall := range rawCalls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		callID, _ := call["id"].(string)
		arguments, _ := function["arguments"].(string)
		calls = append(calls, map[string]any{
			"type":      "function_call",
			"call_id":   clampCallID(callID),
			"name":      name,
			"arguments": arguments,
		})
	}
	return calls
}

func chatReasoningItem(message map[string]any) map[string]any {
	encrypted, _ := message["encrypted_content"].(string)
	if encrypted == "" {
		encrypted, _ = message["reasoning_encrypted_content"].(string)
	}
	summary, _ := message["reasoning_content"].(string)
	if encrypted == "" && summary == "" {
		return nil
	}
	item := map[string]any{"type": "reasoning"}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	if summary != "" {
		item["summary"] = []any{
			map[string]any{"type": "summary_text", "text": summary},
		}
	}
	return item
}

func clampCallID(value string) string {
	if len(value) > 64 {
		return value[:64]
	}
	return value
}

func textContent(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func newChatState(model string) *chatState {
	id, err := secureSessionID()
	if err != nil {
		id = fmt.Sprintf("rute-%d", time.Now().UnixNano())
	}
	return &chatState{
		id:      "chatcmpl-" + strings.TrimPrefix(id, "rute-"),
		model:   model,
		created: time.Now().Unix(),
	}
}

func collectChat(
	ctx context.Context,
	reader io.Reader,
	state *chatState,
	emit func(map[string]any) error,
) error {
	return readSSE(ctx, reader, func(event sseEvent) error {
		var payload map[string]any
		if json.Unmarshal(event.data, &payload) != nil {
			return nil
		}
		eventType, _ := payload["type"].(string)
		if eventType == "" {
			eventType = event.event
		}
		switch eventType {
		case "response.output_text.delta":
			delta, _ := payload["delta"].(string)
			if delta == "" {
				return nil
			}
			state.content.WriteString(delta)
			return emitIfSet(emit, state.chunk(map[string]any{"content": delta}, nil))
		case "response.reasoning_summary_text.delta":
			delta, _ := payload["delta"].(string)
			if delta == "" {
				return nil
			}
			return emitIfSet(emit, state.chunk(map[string]any{"reasoning_content": delta}, nil))
		case "response.output_item.added":
			item, _ := payload["item"].(map[string]any)
			if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
				return nil
			}
			call := &chatToolCall{}
			call.id, _ = item["call_id"].(string)
			call.name, _ = item["name"].(string)
			state.toolCalls = append(state.toolCalls, call)
			index := len(state.toolCalls) - 1
			return emitIfSet(emit, state.chunk(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": index,
						"id":    call.id,
						"type":  "function",
						"function": map[string]any{
							"name":      call.name,
							"arguments": "",
						},
					},
				},
			}, nil))
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			if len(state.toolCalls) == 0 {
				return nil
			}
			delta, _ := payload["delta"].(string)
			call := state.toolCalls[len(state.toolCalls)-1]
			call.arguments.WriteString(delta)
			return emitIfSet(emit, state.chunk(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": len(state.toolCalls) - 1,
						"function": map[string]any{
							"arguments": delta,
						},
					},
				},
			}, nil))
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
				return nil
			}
			call := state.findToolCall(item)
			if call == nil {
				call = &chatToolCall{}
				call.id, _ = item["call_id"].(string)
				call.name, _ = item["name"].(string)
				state.toolCalls = append(state.toolCalls, call)
			}
			if call.arguments.Len() == 0 {
				arguments, _ := item["arguments"].(string)
				if arguments == "" {
					arguments, _ = item["input"].(string)
				}
				call.arguments.WriteString(arguments)
			}
		case "response.completed", "response.done":
			state.completed = true
			if response, ok := payload["response"].(map[string]any); ok {
				state.readCompletedResponse(response)
			}
		case "response.failed", "error":
			state.failed = true
		}
		return nil
	})
}

func (state *chatState) findToolCall(item map[string]any) *chatToolCall {
	callID, _ := item["call_id"].(string)
	for _, call := range state.toolCalls {
		if callID != "" && call.id == callID {
			return call
		}
	}
	return nil
}

func (state *chatState) readCompletedResponse(response map[string]any) {
	if usage, ok := response["usage"].(map[string]any); ok {
		state.usage = chatUsage(usage)
	}
	if state.content.Len() > 0 && len(state.toolCalls) > 0 {
		return
	}
	output, _ := response["output"].([]any)
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "message":
			if state.content.Len() > 0 {
				continue
			}
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if part["type"] == "output_text" {
					text, _ := part["text"].(string)
					state.content.WriteString(text)
				}
			}
		case "function_call", "custom_tool_call":
			if len(state.toolCalls) > 0 {
				continue
			}
			call := &chatToolCall{}
			call.id, _ = item["call_id"].(string)
			call.name, _ = item["name"].(string)
			arguments, _ := item["arguments"].(string)
			call.arguments.WriteString(arguments)
			state.toolCalls = append(state.toolCalls, call)
		}
	}
}

func chatUsage(usage map[string]any) map[string]any {
	input := numericValue(usage["input_tokens"])
	output := numericValue(usage["output_tokens"])
	result := map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result["prompt_tokens_details"] = map[string]any{
			"cached_tokens": numericValue(details["cached_tokens"]),
		}
	}
	return result
}

func numericValue(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		number, _ := typed.Int64()
		return number
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func (state *chatState) chunk(delta map[string]any, finishReason any) map[string]any {
	return map[string]any{
		"id":      state.id,
		"object":  "chat.completion.chunk",
		"created": state.created,
		"model":   state.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
}

func (state *chatState) completion() map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": state.content.String(),
	}
	finishReason := "stop"
	if len(state.toolCalls) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = state.toolCallValues()
		if state.content.Len() == 0 {
			message["content"] = nil
		}
	}
	result := map[string]any{
		"id":      state.id,
		"object":  "chat.completion",
		"created": state.created,
		"model":   state.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if state.usage != nil {
		result["usage"] = state.usage
	}
	return result
}

func (state *chatState) toolCallValues() []any {
	values := make([]any, 0, len(state.toolCalls))
	for _, call := range state.toolCalls {
		values = append(values, map[string]any{
			"id":   call.id,
			"type": "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments.String(),
			},
		})
	}
	return values
}

func (handler *Handler) streamChat(
	writer http.ResponseWriter,
	request *http.Request,
	reader io.Reader,
	state *chatState,
) {
	writer.Header().Set("Cache-Control", "no-cache, no-store")
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(writer)

	emit := func(chunk map[string]any) error {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := emit(state.chunk(map[string]any{"role": "assistant"}, nil)); err != nil {
		return
	}
	err := collectChat(request.Context(), reader, state, emit)
	if err != nil || state.failed || !state.completed {
		_ = emit(map[string]any{
			"error": apiError{
				Message: "Codex upstream stream failed",
				Type:    "upstream_error",
				Code:    "stream_error",
			},
		})
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		_ = controller.Flush()
		return
	}

	finishReason := "stop"
	if len(state.toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	final := state.chunk(map[string]any{}, finishReason)
	if state.usage != nil {
		final["usage"] = state.usage
	}
	_ = emit(final)
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	_ = controller.Flush()
}

func emitIfSet(emit func(map[string]any) error, value map[string]any) error {
	if emit == nil {
		return nil
	}
	return emit(value)
}
