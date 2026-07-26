package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const defaultInstructions = "You are Codex, a coding agent. Follow the user's instructions carefully."

var responsesAllowlist = map[string]struct{}{
	"client_metadata":  {},
	"include":          {},
	"input":            {},
	"instructions":     {},
	"model":            {},
	"prompt_cache_key": {},
	"reasoning":        {},
	"service_tier":     {},
	"store":            {},
	"stream":           {},
	"text":             {},
	"tool_choice":      {},
	"tools":            {},
}

var hostedToolTypes = map[string]struct{}{
	"code_interpreter":     {},
	"computer":             {},
	"computer_use_preview": {},
	"file_search":          {},
	"image_generation":     {},
	"local_shell":          {},
	"mcp":                  {},
	"tool_search":          {},
	"web_search":           {},
	"web_search_preview":   {},
}

func normalizeResponses(body map[string]any, defaultModel, sessionID string) (map[string]any, error) {
	normalized := make(map[string]any, len(responsesAllowlist))
	for key, value := range body {
		if _, allowed := responsesAllowlist[key]; allowed {
			normalized[key] = value
		}
	}

	model, _ := normalized["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	normalized["model"] = stripEffortSuffix(model, normalized)
	normalized["input"] = normalizeInput(normalized["input"])
	normalized["stream"] = true
	normalized["store"] = false

	instructions, _ := normalized["instructions"].(string)
	if strings.TrimSpace(instructions) == "" {
		normalized["instructions"] = defaultInstructions
	}
	if _, ok := normalized["prompt_cache_key"].(string); !ok {
		normalized["prompt_cache_key"] = sessionID
	}

	normalizeReasoning(normalized)
	normalizeTools(normalized)
	normalizeServiceTier(normalized)
	return normalized, nil
}

func normalizeInput(value any) []any {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			typed = "..."
		}
		return []any{messageItem("user", "input_text", typed)}
	case []any:
		items := make([]any, 0, len(typed))
		for _, rawItem := range typed {
			item, ok := rawItem.(map[string]any)
			if !ok {
				if text, textOK := rawItem.(string); textOK && !serverItemID(text) {
					items = append(items, messageItem("user", "input_text", text))
				}
				continue
			}
			if item["type"] == "item_reference" {
				continue
			}
			if id, ok := item["id"].(string); ok && serverItemID(id) {
				delete(item, "id")
			}
			if item["role"] == "system" {
				item["role"] = "developer"
			}
			normalizeMessageContent(item)
			items = append(items, item)
		}
		if len(items) > 0 {
			return items
		}
	}
	return []any{messageItem("user", "input_text", "...")}
}

func normalizeMessageContent(item map[string]any) {
	role, _ := item["role"].(string)
	content, ok := item["content"].(string)
	if !ok {
		return
	}
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	item["content"] = []any{
		map[string]any{
			"type": contentType,
			"text": content,
		},
	}
	if _, ok := item["type"]; !ok {
		item["type"] = "message"
	}
}

func normalizeReasoning(body map[string]any) {
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		reasoning = map[string]any{
			"effort":  "low",
			"summary": "auto",
		}
		body["reasoning"] = reasoning
	}
	effort, _ := reasoning["effort"].(string)
	if effort == "" {
		effort = "low"
	}
	if effort == "max" {
		effort = "xhigh"
	}
	reasoning["effort"] = effort
	if _, ok := reasoning["summary"].(string); !ok {
		reasoning["summary"] = "auto"
	}
	if effort != "none" {
		body["include"] = []any{"reasoning.encrypted_content"}
	}
}

func stripEffortSuffix(model string, body map[string]any) string {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		suffix := "-" + effort
		if !strings.HasSuffix(model, suffix) {
			continue
		}
		if _, exists := body["reasoning"]; !exists {
			body["reasoning"] = map[string]any{
				"effort":  effort,
				"summary": "auto",
			}
		}
		return strings.TrimSuffix(model, suffix)
	}
	return model
}

func normalizeTools(body map[string]any) {
	rawTools, ok := body["tools"].([]any)
	if !ok {
		return
	}
	tools := make([]any, 0, len(rawTools))
	validNames := make(map[string]struct{})
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType == "function" {
			flat, name := flattenFunctionTool(tool)
			if name == "" {
				continue
			}
			validNames[name] = struct{}{}
			tools = append(tools, flat)
			continue
		}
		if toolType == "custom" {
			tools = append(tools, tool)
			continue
		}
		if _, allowed := hostedToolTypes[toolType]; allowed {
			tools = append(tools, tool)
		}
	}
	body["tools"] = tools

	choice, ok := body["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "function" {
		return
	}
	name, _ := choice["name"].(string)
	if name == "" {
		if function, functionOK := choice["function"].(map[string]any); functionOK {
			name, _ = function["name"].(string)
		}
	}
	if _, valid := validNames[name]; !valid {
		delete(body, "tool_choice")
		return
	}
	body["tool_choice"] = map[string]any{
		"type": "function",
		"name": name,
	}
}

func flattenFunctionTool(tool map[string]any) (map[string]any, string) {
	function, _ := tool["function"].(map[string]any)
	name, _ := tool["name"].(string)
	if name == "" {
		name, _ = function["name"].(string)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ""
	}
	if len(name) > 128 {
		name = name[:128]
	}
	description, _ := tool["description"].(string)
	if description == "" {
		description, _ = function["description"].(string)
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok {
		parameters, _ = function["parameters"].(map[string]any)
	}
	if parameters == nil {
		parameters = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	if parameters["type"] == "object" {
		if _, ok := parameters["properties"]; !ok {
			parameters["properties"] = map[string]any{}
		}
	}
	flat := map[string]any{
		"type":       "function",
		"name":       name,
		"parameters": parameters,
	}
	if description != "" {
		flat["description"] = description
	}
	if strict, ok := function["strict"].(bool); ok {
		flat["strict"] = strict
	}
	return flat, name
}

func normalizeServiceTier(body map[string]any) {
	tier, _ := body["service_tier"].(string)
	switch tier {
	case "fast":
		body["service_tier"] = "priority"
	case "priority":
	default:
		delete(body, "service_tier")
	}
}

func messageItem(role, contentType, text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": role,
		"content": []any{
			map[string]any{
				"type": contentType,
				"text": text,
			},
		},
	}
}

func serverItemID(value string) bool {
	for _, prefix := range []string{"rs_", "fc_", "resp_", "msg_"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func secureSessionID() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("proxy: generate session ID: %w", err)
	}
	return "rute-" + base64.RawURLEncoding.EncodeToString(value), nil
}

func requireObject(value any, field string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return object, nil
}

var errUnsupportedWebSocketEvent = errors.New("only response.create events are supported")
