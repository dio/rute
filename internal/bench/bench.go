// Package bench drives closed-loop concurrent inference calls and measures
// time to first token and output speed for each call.
package bench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/rute/internal/latency"
)

// Supported APIs.
const (
	APIMessages    = "messages"
	APIChat        = "chat"
	APIResponses   = "responses"
	APIResponsesWS = "responses-ws"
)

const maxEventBytes = 32 << 20

// Config describes a bench run.
type Config struct {
	// BaseURL is the server root, such as http://127.0.0.1:20128.
	BaseURL string
	API     string
	Model   string
	// Levels lists the worker counts to run, in order.
	Levels []int
	// Rounds is the number of sequential calls each worker makes per level.
	Rounds    int
	Prompt    string
	MaxTokens int
	Effort    string
	Timeout   time.Duration
	Header    http.Header
	Client    *http.Client
	// Record, when set, receives each record as soon as its call ends.
	Record func(latency.Record)
}

// Validate checks cfg and fills defaults.
func (cfg *Config) Validate() error {
	switch cfg.API {
	case APIMessages, APIChat, APIResponses, APIResponsesWS:
	default:
		return fmt.Errorf("api must be %s, %s, %s, or %s",
			APIMessages, APIChat, APIResponses, APIResponsesWS)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return errors.New("model is required")
	}
	if len(cfg.Levels) == 0 {
		return errors.New("at least one concurrency level is required")
	}
	if cfg.Rounds < 1 {
		return errors.New("rounds must be positive")
	}
	if cfg.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	cfg.BaseURL = strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Header == nil {
		cfg.Header = http.Header{}
	}
	return nil
}

// Run executes every level and returns all records.
func Run(ctx context.Context, cfg Config) ([]latency.Record, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var (
		mu      sync.Mutex
		records []latency.Record
	)
	emit := func(record latency.Record) {
		mu.Lock()
		records = append(records, record)
		mu.Unlock()
		if cfg.Record != nil {
			cfg.Record(record)
		}
	}
	runID := randomID()
	for _, level := range cfg.Levels {
		if err := ctx.Err(); err != nil {
			return records, err
		}
		var (
			inFlight atomic.Int64
			wait     sync.WaitGroup
		)
		for worker := range level {
			wait.Go(func() {
				client := &workerClient{
					cfg:       cfg,
					level:     level,
					inFlight:  &inFlight,
					sessionID: fmt.Sprintf("bench-%s-%d-%d", runID, level, worker),
				}
				defer client.close()
				for range cfg.Rounds {
					if ctx.Err() != nil {
						return
					}
					emit(client.call(ctx))
				}
			})
		}
		wait.Wait()
	}
	return records, ctx.Err()
}

type workerClient struct {
	cfg       Config
	level     int
	inFlight  *atomic.Int64
	sessionID string
	conn      *websocket.Conn
}

func (client *workerClient) close() {
	if client.conn != nil {
		_ = client.conn.Close(websocket.StatusNormalClosure, "")
	}
}

func (client *workerClient) call(parent context.Context) latency.Record {
	ctx, cancel := context.WithTimeout(parent, client.cfg.Timeout)
	defer cancel()
	record := latency.Record{
		Source:      "bench",
		Endpoint:    client.cfg.API,
		Model:       client.cfg.Model,
		Concurrency: client.level,
		InFlight:    int(client.inFlight.Add(1)),
		Attempts:    1,
	}
	defer client.inFlight.Add(-1)
	observer := latency.NewObserver()
	start := time.Now()
	var err error
	if client.cfg.API == APIResponsesWS {
		err = client.webSocketCall(ctx, &record, observer)
	} else {
		err = client.httpCall(ctx, &record, observer)
	}
	end := time.Now()
	observer.Finish()
	record.Time = start.UTC()
	record.Fill(start, end, observer)
	switch {
	case record.Outcome != "":
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil:
		record.Outcome = latency.OutcomeTimeout
	case parent.Err() != nil:
		record.Outcome = latency.OutcomeCanceled
	case observer.Failed():
		record.Outcome = latency.OutcomeUpstreamError
	case err != nil:
		record.Outcome = latency.OutcomeError
	case observer.Completed():
		record.Outcome = latency.OutcomeOK
	default:
		record.Outcome = latency.OutcomeIncomplete
	}
	return record
}

func (client *workerClient) httpCall(
	ctx context.Context,
	record *latency.Record,
	observer *latency.Observer,
) error {
	path, body := client.httpBody()
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.cfg.BaseURL+path,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	for key, values := range client.cfg.Header {
		request.Header[key] = values
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-Session-ID", client.sessionID)
	if client.cfg.API == APIMessages && request.Header.Get("Anthropic-Version") == "" {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	response, err := client.cfg.Client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	record.Status = response.StatusCode
	if response.StatusCode == http.StatusTooManyRequests {
		record.Outcome = latency.OutcomeRateLimited
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		record.Outcome = latency.OutcomeUpstreamError
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	_, err = io.Copy(observer, response.Body)
	return err
}

func (client *workerClient) httpBody() (string, map[string]any) {
	cfg := client.cfg
	switch cfg.API {
	case APIMessages:
		return "/v1/messages", map[string]any{
			"model":      cfg.Model,
			"max_tokens": cfg.MaxTokens,
			"stream":     true,
			"messages":   []any{map[string]any{"role": "user", "content": cfg.Prompt}},
		}
	case APIChat:
		body := map[string]any{
			"model":          cfg.Model,
			"max_tokens":     cfg.MaxTokens,
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"messages":       []any{map[string]any{"role": "user", "content": cfg.Prompt}},
		}
		if cfg.Effort != "" {
			body["reasoning_effort"] = cfg.Effort
		}
		return "/v1/chat/completions", body
	default:
		body := client.responsesBody()
		body["stream"] = true
		return "/v1/responses", body
	}
}

func (client *workerClient) responsesBody() map[string]any {
	body := map[string]any{
		"model": client.cfg.Model,
		"input": client.cfg.Prompt,
	}
	if client.cfg.Effort != "" {
		body["reasoning"] = map[string]any{"effort": client.cfg.Effort}
	}
	return body
}

func (client *workerClient) webSocketCall(
	ctx context.Context,
	record *latency.Record,
	observer *latency.Observer,
) error {
	if client.conn == nil {
		header := client.cfg.Header.Clone()
		header.Set("X-Session-ID", client.sessionID)
		url := "ws" + strings.TrimPrefix(client.cfg.BaseURL, "http") + "/v1/responses"
		conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPClient: client.cfg.Client,
			HTTPHeader: header,
		})
		if response != nil {
			record.Status = response.StatusCode
		}
		if err != nil {
			return err
		}
		conn.SetReadLimit(maxEventBytes)
		client.conn = conn
	}
	event := client.responsesBody()
	event["type"] = "response.create"
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := client.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		client.dropConnection()
		return err
	}
	for {
		_, message, err := client.conn.Read(ctx)
		if err != nil {
			client.dropConnection()
			return err
		}
		observer.Event(message, time.Now())
		var header struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(message, &header)
		switch header.Type {
		case "response.completed", "response.done", "response.incomplete",
			"response.failed", "error":
			if header.Type == "error" && bytes.Contains(message, []byte("rate_limit")) {
				record.Outcome = latency.OutcomeRateLimited
			}
			return nil
		}
	}
}

// dropConnection discards a connection whose state is unknown after a failed
// read or write. The next call dials again.
func (client *workerClient) dropConnection() {
	if client.conn != nil {
		client.conn.CloseNow()
		client.conn = nil
	}
}

func randomID() string {
	var buffer [4]byte
	_, _ = rand.Read(buffer[:])
	return hex.EncodeToString(buffer[:])
}
