package latency

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

const (
	maxPendingLine = 32 << 20
	maxJSONBody    = 1 << 20
)

// Observer finds the first generated token, the output token count, and the
// terminal state in a Codex Responses, Claude Messages, or Chat Completions
// stream. Feed it raw SSE bytes with Write, or individual JSON events with
// Event. It is not safe for concurrent use.
type Observer struct {
	now func() time.Time

	mode    byte // 0 undecided, 's' SSE, 'j' JSON body
	pending []byte
	body    []byte
	dropped bool

	firstByte  time.Time
	firstToken time.Time
	tokens     int64
	completed  bool
	failed     bool
}

// NewObserver returns an Observer that reads the wall clock.
func NewObserver() *Observer {
	return &Observer{now: time.Now}
}

// Write observes raw response bytes. It never returns an error.
func (observer *Observer) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	now := observer.now()
	if observer.firstByte.IsZero() {
		observer.firstByte = now
	}
	if observer.mode == 0 {
		trimmed := bytes.TrimLeft(data, " \t\r\n")
		if len(trimmed) == 0 {
			return len(data), nil
		}
		if trimmed[0] == '{' {
			observer.mode = 'j'
		} else {
			observer.mode = 's'
		}
	}
	if observer.mode == 'j' {
		if len(observer.body)+len(data) <= maxJSONBody {
			observer.body = append(observer.body, data...)
		} else {
			observer.dropped = true
		}
		return len(data), nil
	}
	observer.pending = append(observer.pending, data...)
	for {
		index := bytes.IndexByte(observer.pending, '\n')
		if index < 0 {
			break
		}
		observer.line(observer.pending[:index], now)
		observer.pending = observer.pending[index+1:]
	}
	if len(observer.pending) > maxPendingLine {
		observer.pending = nil
		observer.dropped = true
	}
	return len(data), nil
}

// Finish processes any buffered data. Call it once the body is exhausted.
func (observer *Observer) Finish() {
	now := observer.now()
	switch observer.mode {
	case 's':
		if len(observer.pending) > 0 {
			observer.line(observer.pending, now)
			observer.pending = nil
		}
	case 'j':
		if !observer.dropped && len(observer.body) > 0 {
			observer.event(observer.body, now)
			observer.body = nil
		}
	}
}

func (observer *Observer) line(line []byte, now time.Time) {
	line = bytes.TrimRight(line, "\r")
	value, found := bytes.CutPrefix(line, []byte("data:"))
	if !found {
		return
	}
	value = bytes.TrimSpace(value)
	if bytes.Equal(value, []byte("[DONE]")) {
		observer.completed = true
		return
	}
	if len(value) > 0 && value[0] == '{' {
		observer.Event(value, now)
	}
}

type usage struct {
	OutputTokens     *int64 `json:"output_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

type event struct {
	Type     string          `json:"type"`
	Error    json.RawMessage `json:"error"`
	Usage    *usage          `json:"usage"`
	Response *struct {
		Usage *usage `json:"usage"`
	} `json:"response"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
	Choices []struct {
		Delta        map[string]json.RawMessage `json:"delta"`
		FinishReason *string                    `json:"finish_reason"`
	} `json:"choices"`
}

// Event observes one JSON event received at time at.
func (observer *Observer) Event(data []byte, at time.Time) {
	observer.event(data, at)
}

func (observer *Observer) event(data []byte, at time.Time) {
	if observer.firstByte.IsZero() {
		observer.firstByte = at
	}
	var value event
	if json.Unmarshal(data, &value) != nil {
		return
	}
	if len(value.Error) > 0 && !bytes.Equal(value.Error, []byte("null")) {
		observer.failed = true
	}
	switch {
	case value.Type == "error", value.Type == "response.failed":
		observer.failed = true
	case value.Type == "content_block_delta", strings.HasSuffix(value.Type, ".delta"):
		observer.token(at)
	case value.Type == "response.completed",
		value.Type == "response.done",
		value.Type == "response.incomplete",
		value.Type == "message_stop":
		observer.completed = true
	}
	for _, choice := range value.Choices {
		if chatDeltaHasOutput(choice.Delta) {
			observer.token(at)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			observer.completed = true
		}
	}
	observer.usage(value.Usage)
	if value.Response != nil {
		observer.usage(value.Response.Usage)
	}
	if value.Message != nil {
		observer.usage(value.Message.Usage)
	}
	if observer.mode == 'j' && observer.tokens > 0 && !observer.failed {
		observer.completed = true
	}
}

func chatDeltaHasOutput(delta map[string]json.RawMessage) bool {
	for _, key := range []string{"content", "tool_calls", "reasoning_content", "reasoning"} {
		raw, ok := delta[key]
		if !ok {
			continue
		}
		switch string(bytes.TrimSpace(raw)) {
		case "", "null", `""`, "[]":
			continue
		}
		return true
	}
	return false
}

func (observer *Observer) token(at time.Time) {
	if observer.firstToken.IsZero() {
		observer.firstToken = at
	}
}

func (observer *Observer) usage(value *usage) {
	if value == nil {
		return
	}
	for _, count := range []*int64{value.OutputTokens, value.CompletionTokens} {
		if count != nil && *count > observer.tokens {
			observer.tokens = *count
		}
	}
}

// Streamed reports whether the body was an event stream rather than a single
// JSON document.
func (observer *Observer) Streamed() bool {
	return observer.mode != 'j'
}

// FirstToken returns when the first generated token arrived. For a JSON body,
// which arrives all at once, it returns the first byte time.
func (observer *Observer) FirstToken() time.Time {
	if observer.mode == 'j' {
		return observer.firstByte
	}
	return observer.firstToken
}

// OutputTokens returns the largest output token count reported upstream.
func (observer *Observer) OutputTokens() int64 {
	return observer.tokens
}

// Completed reports whether a terminal success event was seen.
func (observer *Observer) Completed() bool {
	return observer.completed && !observer.failed
}

// Failed reports whether an error event was seen.
func (observer *Observer) Failed() bool {
	return observer.failed
}
