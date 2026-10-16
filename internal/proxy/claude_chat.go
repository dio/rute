package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
)

const defaultClaudeMaxTokens = 8192

type claudeChatStreamState struct {
	id               string
	model            string
	created          int64
	nextToolIndex    int
	toolIndexes      map[int]int
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	finishSent       bool
	doneSent         bool
	includeUsage     bool
}

func (handler *Handler) handleClaudeChatBody(
	writer http.ResponseWriter,
	request *http.Request,
	body map[string]any,
	publicModel string,
	sessionID string,
	stream bool,
) {
	translated, includeUsage, err := openAIToClaudeChat(body, stream)
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
	payload, err := json.Marshal(translated)
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalid request",
			"invalid_request_error",
			"invalid_request",
		)
		return
	}
	response, err := handler.claudeUpstream(
		request.Context(),
		payload,
		request.Header,
		sessionID,
	)
	if err != nil {
		writeProxyErrorForProvider(writer, err, "Claude")
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstreamErr := claudeChatUpstreamError(response)
		writeJSON(writer, response.StatusCode, map[string]any{"error": upstreamErr})
		return
	}
	if stream {
		state := &claudeChatStreamState{
			model:        publicModel,
			created:      time.Now().Unix(),
			toolIndexes:  make(map[int]int),
			includeUsage: includeUsage,
		}
		if err := streamClaudeChat(request.Context(), writer, response.Body, state); err != nil {
			return
		}
		return
	}
	completion, err := collectClaudeChat(response.Body, publicModel)
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadGateway,
			"Claude upstream returned an invalid response",
			"upstream_error",
			"invalid_upstream_response",
		)
		return
	}
	writeJSON(writer, http.StatusOK, completion)
}

func openAIToClaudeChat(body map[string]any, stream bool) (map[string]any, bool, error) {
	for _, field := range []string{
		"audio",
		"logprobs",
		"logit_bias",
		"response_format",
		"frequency_penalty",
		"presence_penalty",
	} {
		if _, exists := body[field]; exists {
			return nil, false, fmt.Errorf("%s is not supported for Claude models", field)
		}
	}
	if rawN, present := body["n"]; present {
		n, valid := integerField(rawN)
		if !valid || n != 1 {
			return nil, false, errors.New("n must be 1 for Claude models")
		}
	}
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, false, errors.New("model is required")
	}
	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, false, errors.New("messages must be a non-empty array")
	}

	result := map[string]any{
		"model":      model,
		"max_tokens": int64(defaultClaudeMaxTokens),
		"stream":     stream,
	}
	systemParts := make([]string, 0)
	messages := make([]map[string]any, 0, len(rawMessages))
	for index, rawMessage := range rawMessages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("messages[%d] must be an object", index)
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			text, err := openAITextContent(message["content"])
			if err != nil {
				return nil, false, fmt.Errorf("messages[%d]: %w", index, err)
			}
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			content, err := openAIUserContent(message["content"])
			if err != nil {
				return nil, false, fmt.Errorf("messages[%d]: %w", index, err)
			}
			appendClaudeMessage(&messages, "user", content)
		case "assistant":
			content, err := openAIAssistantContent(message)
			if err != nil {
				return nil, false, fmt.Errorf("messages[%d]: %w", index, err)
			}
			appendClaudeMessage(&messages, "assistant", content)
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if strings.TrimSpace(callID) == "" {
				return nil, false, fmt.Errorf("messages[%d] tool message requires tool_call_id", index)
			}
			content := textContent(message["content"])
			appendClaudeMessage(&messages, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     content,
			}})
		default:
			return nil, false, fmt.Errorf("messages[%d] has unsupported role %q", index, role)
		}
	}
	if len(messages) == 0 {
		return nil, false, errors.New("messages did not contain user or assistant content")
	}
	result["messages"] = messages
	if len(systemParts) > 0 {
		result["system"] = strings.Join(systemParts, "\n\n")
	}

	if value, exists := body["max_completion_tokens"]; exists {
		maximum, ok := positiveInteger(value)
		if !ok {
			return nil, false, errors.New("max_completion_tokens must be a positive integer")
		}
		result["max_tokens"] = maximum
	} else if value, exists := body["max_tokens"]; exists {
		maximum, ok := positiveInteger(value)
		if !ok {
			return nil, false, errors.New("max_tokens must be a positive integer")
		}
		result["max_tokens"] = maximum
	}
	for _, field := range []string{"temperature", "top_p", "metadata"} {
		if value, exists := body[field]; exists {
			result[field] = value
		}
	}
	if stop, exists := body["stop"]; exists {
		sequences, err := stopSequences(stop)
		if err != nil {
			return nil, false, err
		}
		result["stop_sequences"] = sequences
	}
	if rawTools, exists := body["tools"]; exists {
		tools, err := openAITools(rawTools)
		if err != nil {
			return nil, false, err
		}
		result["tools"] = tools
	}
	if choice, exists := body["tool_choice"]; exists {
		translated, err := openAIToolChoice(choice)
		if err != nil {
			return nil, false, err
		}
		result["tool_choice"] = translated
	}
	includeUsage := false
	if options, ok := body["stream_options"].(map[string]any); ok {
		includeUsage, _ = options["include_usage"].(bool)
	}
	return result, includeUsage, nil
}

func openAIUserContent(value any) ([]any, error) {
	switch typed := value.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": typed}}, nil
	case []any:
		content := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, errors.New("content parts must be objects")
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text":
				text, _ := part["text"].(string)
				content = append(content, map[string]any{"type": "text", "text": text})
			case "image_url":
				source, err := claudeImageSource(part["image_url"])
				if err != nil {
					return nil, err
				}
				content = append(content, map[string]any{"type": "image", "source": source})
			default:
				return nil, fmt.Errorf("unsupported user content type %q", partType)
			}
		}
		return content, nil
	default:
		return nil, errors.New("content must be a string or array")
	}
}

func openAIAssistantContent(message map[string]any) ([]any, error) {
	content := make([]any, 0)
	if value, exists := message["content"]; exists && value != nil {
		text, err := openAITextContent(value)
		if err != nil {
			return nil, err
		}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	}
	if rawCalls, exists := message["tool_calls"]; exists {
		calls, ok := rawCalls.([]any)
		if !ok {
			return nil, errors.New("tool_calls must be an array")
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				return nil, errors.New("tool call must be an object")
			}
			callID, _ := call["id"].(string)
			function, _ := call["function"].(map[string]any)
			name, _ := function["name"].(string)
			arguments, _ := function["arguments"].(string)
			if callID == "" || name == "" {
				return nil, errors.New("tool call requires id and function.name")
			}
			var input map[string]any
			if err := json.Unmarshal([]byte(arguments), &input); err != nil {
				return nil, fmt.Errorf("tool call %q arguments must be a JSON object", callID)
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    callID,
				"name":  name,
				"input": input,
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return content, nil
}

func openAITextContent(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []any:
		var builder strings.Builder
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return "", errors.New("content parts must be objects")
			}
			partType, _ := part["type"].(string)
			if partType != "text" && partType != "input_text" && partType != "output_text" {
				return "", fmt.Errorf("unsupported text content type %q", partType)
			}
			text, _ := part["text"].(string)
			builder.WriteString(text)
		}
		return builder.String(), nil
	default:
		return "", errors.New("content must be a string or text array")
	}
}

func appendClaudeMessage(messages *[]map[string]any, role string, content []any) {
	if len(*messages) > 0 {
		last := &(*messages)[len(*messages)-1]
		if (*last)["role"] == role {
			existing, _ := (*last)["content"].([]any)
			(*last)["content"] = append(existing, content...)
			return
		}
	}
	*messages = append(*messages, map[string]any{"role": role, "content": content})
}

func claudeImageSource(value any) (map[string]any, error) {
	rawURL := ""
	switch typed := value.(type) {
	case string:
		rawURL = typed
	case map[string]any:
		rawURL, _ = typed["url"].(string)
	}
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		return map[string]any{"type": "url", "url": rawURL}, nil
	}
	if !strings.HasPrefix(rawURL, "data:") {
		return nil, errors.New("image_url must be HTTP(S) or a base64 data URI")
	}
	header, encoded, found := strings.Cut(strings.TrimPrefix(rawURL, "data:"), ",")
	if !found || !strings.HasSuffix(header, ";base64") {
		return nil, errors.New("image data URI must use base64 encoding")
	}
	mediaType := strings.TrimSuffix(header, ";base64")
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return nil, fmt.Errorf("unsupported image media type %q", mediaType)
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return nil, errors.New("image data URI contains invalid base64")
	}
	return map[string]any{
		"type":       "base64",
		"media_type": mediaType,
		"data":       encoded,
	}, nil
}

func openAITools(value any) ([]any, error) {
	rawTools, ok := value.([]any)
	if !ok {
		return nil, errors.New("tools must be an array")
	}
	tools := make([]any, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]any)
		if !ok || tool["type"] != "function" {
			return nil, errors.New("claude chat supports function tools only")
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			return nil, errors.New("function tool requires a function object")
		}
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("function tool requires a name")
		}
		schema, _ := function["parameters"].(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		translated := map[string]any{
			"name":         name,
			"input_schema": schema,
		}
		if description, _ := function["description"].(string); description != "" {
			translated["description"] = description
		}
		tools = append(tools, translated)
	}
	return tools, nil
}

func openAIToolChoice(value any) (map[string]any, error) {
	switch typed := value.(type) {
	case string:
		switch typed {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "none":
			return map[string]any{"type": "none"}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", typed)
		}
	case map[string]any:
		function, _ := typed["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			return nil, errors.New("function tool_choice requires function.name")
		}
		return map[string]any{"type": "tool", "name": name}, nil
	default:
		return nil, errors.New("tool_choice must be a string or object")
	}
}

func stopSequences(value any) ([]string, error) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, nil
	case []any:
		sequences := make([]string, 0, len(typed))
		for _, raw := range typed {
			sequence, ok := raw.(string)
			if !ok {
				return nil, errors.New("stop must contain strings")
			}
			sequences = append(sequences, sequence)
		}
		return sequences, nil
	default:
		return nil, errors.New("stop must be a string or array of strings")
	}
}

func positiveInteger(value any) (int64, bool) {
	integer, exists := integerField(value)
	return integer, exists && integer > 0
}

func integerField(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		integer, err := typed.Int64()
		return integer, err == nil
	case float64:
		integer := int64(typed)
		return integer, float64(integer) == typed
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func collectClaudeChat(reader io.Reader, publicModel string) (map[string]any, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, defaultMaxRequestBytes+1))
	decoder.UseNumber()
	var message map[string]any
	if err := decoder.Decode(&message); err != nil {
		return nil, err
	}
	id, _ := message["id"].(string)
	content, _ := message["content"].([]any)
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]any, 0)
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			value, _ := block["text"].(string)
			text.WriteString(value)
		case "thinking":
			value, _ := block["thinking"].(string)
			reasoning.WriteString(value)
		case "tool_use":
			arguments, _ := json.Marshal(block["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id":   block["id"],
				"type": "function",
				"function": map[string]any{
					"name":      block["name"],
					"arguments": string(arguments),
				},
			})
		}
	}
	assistant := map[string]any{
		"role":    "assistant",
		"content": text.String(),
	}
	if len(toolCalls) > 0 {
		assistant["tool_calls"] = toolCalls
	}
	if reasoning.Len() > 0 {
		assistant["reasoning_content"] = reasoning.String()
	}
	stopReason, _ := message["stop_reason"].(string)
	return map[string]any{
		"id":      chatCompletionID(id),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   publicModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       assistant,
			"finish_reason": openAIFinishReason(stopReason, len(toolCalls) > 0),
		}},
		"usage": claudeUsage(message["usage"]),
	}, nil
}

func streamClaudeChat(
	ctx context.Context,
	writer http.ResponseWriter,
	reader io.Reader,
	state *claudeChatStreamState,
) error {
	writer.Header().Set("Cache-Control", "no-cache, no-store")
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(writer)
	err := readSSE(ctx, reader, func(event sseEvent) error {
		var payload map[string]any
		decoder := json.NewDecoder(bytes.NewReader(event.data))
		decoder.UseNumber()
		if decoder.Decode(&payload) != nil {
			return nil
		}
		eventType, _ := payload["type"].(string)
		switch eventType {
		case "message_start":
			message, _ := payload["message"].(map[string]any)
			state.id, _ = message["id"].(string)
			readClaudeStreamUsage(state, message["usage"])
			return writeOpenAISSE(writer, controller, state.chunk(map[string]any{
				"role": "assistant",
			}, nil, false))
		case "content_block_start":
			index, _ := integerField(payload["index"])
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] != "tool_use" {
				return nil
			}
			toolIndex := state.nextToolIndex
			state.nextToolIndex++
			state.toolIndexes[int(index)] = toolIndex
			return writeOpenAISSE(writer, controller, state.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": toolIndex,
					"id":    block["id"],
					"type":  "function",
					"function": map[string]any{
						"name":      block["name"],
						"arguments": "",
					},
				}},
			}, nil, false))
		case "content_block_delta":
			index, _ := integerField(payload["index"])
			delta, _ := payload["delta"].(map[string]any)
			switch delta["type"] {
			case "text_delta":
				return writeOpenAISSE(writer, controller, state.chunk(map[string]any{
					"content": delta["text"],
				}, nil, false))
			case "thinking_delta":
				return writeOpenAISSE(writer, controller, state.chunk(map[string]any{
					"reasoning_content": delta["thinking"],
				}, nil, false))
			case "input_json_delta":
				toolIndex, ok := state.toolIndexes[int(index)]
				if !ok {
					return nil
				}
				return writeOpenAISSE(writer, controller, state.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": toolIndex,
						"function": map[string]any{
							"arguments": delta["partial_json"],
						},
					}},
				}, nil, false))
			}
		case "message_delta":
			readClaudeStreamUsage(state, payload["usage"])
			delta, _ := payload["delta"].(map[string]any)
			reason, _ := delta["stop_reason"].(string)
			finish := openAIFinishReason(reason, state.nextToolIndex > 0)
			state.finishSent = true
			return writeOpenAISSE(
				writer,
				controller,
				state.chunk(map[string]any{}, &finish, state.includeUsage),
			)
		case "message_stop":
			if !state.finishSent {
				finish := openAIFinishReason("", state.nextToolIndex > 0)
				state.finishSent = true
				if err := writeOpenAISSE(
					writer,
					controller,
					state.chunk(map[string]any{}, &finish, state.includeUsage),
				); err != nil {
					return err
				}
			}
			state.doneSent = true
			return writeOpenAIDone(writer, controller)
		case "error":
			return writeOpenAISSE(writer, controller, map[string]any{
				"error": map[string]any{
					"message": "Claude upstream stream failed",
					"type":    "upstream_error",
					"code":    "upstream_error",
				},
			})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !state.finishSent {
		finish := openAIFinishReason("", state.nextToolIndex > 0)
		if err := writeOpenAISSE(
			writer,
			controller,
			state.chunk(map[string]any{}, &finish, state.includeUsage),
		); err != nil {
			return err
		}
	}
	if state.doneSent {
		return nil
	}
	return writeOpenAIDone(writer, controller)
}

func (state *claudeChatStreamState) chunk(
	delta map[string]any,
	finishReason *string,
	includeUsage bool,
) map[string]any {
	if state.id == "" {
		state.id = randomSessionID()
	}
	chunk := map[string]any{
		"id":      chatCompletionID(state.id),
		"object":  "chat.completion.chunk",
		"created": state.created,
		"model":   state.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	if includeUsage {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     state.inputTokens + state.cacheReadTokens + state.cacheWriteTokens,
			"completion_tokens": state.outputTokens,
			"total_tokens": state.inputTokens +
				state.cacheReadTokens +
				state.cacheWriteTokens +
				state.outputTokens,
		}
	}
	return chunk
}

func readClaudeStreamUsage(state *claudeChatStreamState, value any) {
	usage, _ := value.(map[string]any)
	if number, ok := integerField(usage["input_tokens"]); ok {
		state.inputTokens = number
	}
	if number, ok := integerField(usage["output_tokens"]); ok {
		state.outputTokens = number
	}
	if number, ok := integerField(usage["cache_read_input_tokens"]); ok {
		state.cacheReadTokens = number
	}
	if number, ok := integerField(usage["cache_creation_input_tokens"]); ok {
		state.cacheWriteTokens = number
	}
}

func writeOpenAISSE(
	writer io.Writer,
	controller *http.ResponseController,
	payload map[string]any,
) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
		return err
	}
	return controller.Flush()
}

func writeOpenAIDone(writer io.Writer, controller *http.ResponseController) error {
	if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return controller.Flush()
}

func claudeUsage(value any) map[string]any {
	usage, _ := value.(map[string]any)
	input, _ := integerField(usage["input_tokens"])
	output, _ := integerField(usage["output_tokens"])
	cacheRead, _ := integerField(usage["cache_read_input_tokens"])
	cacheWrite, _ := integerField(usage["cache_creation_input_tokens"])
	prompt := input + cacheRead + cacheWrite
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": output,
		"total_tokens":      prompt + output,
	}
}

func openAIFinishReason(reason string, hasTools bool) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence", "pause_turn", "refusal":
		return "stop"
	default:
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

func chatCompletionID(id string) string {
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + id
}

func claudeChatUpstreamError(response *http.Response) apiError {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes+1))
	if err != nil || len(body) > maxUpstreamErrorBytes {
		return apiError{
			Message: "Claude upstream request failed",
			Type:    "upstream_error",
			Code:    "upstream_error",
		}
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		errorType := envelope.Error.Type
		if errorType == "" {
			errorType = "upstream_error"
		}
		return apiError{
			Message: envelope.Error.Message,
			Type:    errorType,
			Code:    errorType,
		}
	}
	return apiError{
		Message: fmt.Sprintf("Claude upstream returned status %d", response.StatusCode),
		Type:    "upstream_error",
		Code:    "upstream_error",
	}
}

func writeProxyErrorForProvider(writer http.ResponseWriter, err error, provider string) {
	if setRateLimitRetryAfter(writer, err) {
		writeAPIError(writer, http.StatusTooManyRequests,
			"All "+provider+" accounts are rate-limited; retry after the indicated cooldown",
			"rate_limit_error", "rate_limit_exceeded")
		return
	}
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(
			writer,
			http.StatusGatewayTimeout,
			provider+" upstream timed out",
			"upstream_error",
			"upstream_timeout",
		)
	case errors.Is(err, codexpool.ErrAuthenticationRequired),
		errors.Is(err, codexauth.ErrReauthenticationRequired):
		writeAPIError(
			writer,
			http.StatusServiceUnavailable,
			provider+" authentication is required",
			"authentication_error",
			"authentication_required",
		)
	case errors.Is(err, codexpool.ErrAccountsUnavailable):
		writeAPIError(
			writer,
			http.StatusServiceUnavailable,
			"All "+provider+" accounts are temporarily unavailable",
			"upstream_error",
			"upstream_accounts_unavailable",
		)
	default:
		writeAPIError(
			writer,
			http.StatusBadGateway,
			provider+" upstream is unavailable",
			"upstream_error",
			"upstream_unavailable",
		)
	}
}
