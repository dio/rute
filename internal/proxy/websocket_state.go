package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
)

// responseContinuation holds only the latest successful Codex transcript on a
// downstream connection. JSON storage isolates it from request normalization.
type responseContinuation struct {
	id, model string
	input     []byte
}

func (state *responseContinuation) expand(body map[string]any, model string, limit int64) (string, error) {
	previous, exists := body["previous_response_id"]
	if !exists || previous == nil {
		return "", nil
	}
	id, ok := previous.(string)
	if !ok || id == "" {
		return "invalid_request_error", errors.New("previous_response_id must be a non-empty string")
	}
	if id != state.id || model != state.model || len(state.input) == 0 {
		return "previous_response_not_found", errors.New("previous response is unavailable on this connection. Resend the full input without previous_response_id")
	}
	var input []any
	decoder := json.NewDecoder(bytes.NewReader(state.input))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return "previous_response_not_found", errors.New("previous response transcript is unavailable")
	}
	switch delta := body["input"].(type) {
	case []any:
		input = append(input, delta...)
	case string:
		input = append(input, messageItem("user", "input_text", delta))
	case nil:
	default:
		return "invalid_request_error", errors.New("input must be a string or an array")
	}
	body["input"] = input
	delete(body, "previous_response_id")
	data, err := json.Marshal(body)
	if err != nil || int64(len(data)) > limit {
		return "invalid_request_error", errors.New("expanded WebSocket request exceeds the request size limit. Resend a shorter full input")
	}
	return "", nil
}

type responseTurn struct {
	input    []any
	id       string
	output   []any
	failed   bool
	finished bool
}

func (turn *responseTurn) observe(data []byte) {
	var event struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Output []any           `json:"output"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		return
	}
	hasError := func(raw json.RawMessage) bool {
		return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	}
	if event.Type == "error" || event.Type == "response.failed" || hasError(event.Error) {
		turn.failed = true
	}
	if event.Response == nil {
		return
	}
	if hasError(event.Response.Error) {
		turn.failed = true
	}
	if event.Type == "response.completed" || (event.Type == "response.done" && event.Response.Status == "completed") {
		turn.id = event.Response.ID
		turn.output = event.Response.Output
	}
}

func (state *responseContinuation) remember(turn *responseTurn, model string, limit int64) {
	if !turn.finished || turn.failed || turn.id == "" || turn.output == nil {
		return
	}
	input := append(turn.input[:len(turn.input):len(turn.input)], turn.output...)
	data, err := json.Marshal(input)
	// Evict the old response even if the new one is too large to retain.
	*state = responseContinuation{}
	if err != nil || int64(len(data)) > limit {
		return
	}
	*state = responseContinuation{id: turn.id, model: model, input: data}
}
