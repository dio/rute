// Package latency records and summarizes per-call upstream latency so that
// subscription slowdown under concurrency can be measured.
package latency

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Outcomes recorded for a call.
const (
	OutcomeOK            = "ok"
	OutcomeRateLimited   = "rate_limited"
	OutcomeUpstreamError = "upstream_error"
	OutcomeIncomplete    = "incomplete"
	OutcomeCanceled      = "canceled"
	OutcomeTimeout       = "timeout"
	OutcomeError         = "error"
)

// Record describes one inference call. It never contains prompts, outputs,
// or credentials.
type Record struct {
	Time     time.Time `json:"time"`
	Source   string    `json:"source"`
	Provider string    `json:"provider,omitempty"`
	Endpoint string    `json:"endpoint"`
	Model    string    `json:"model,omitempty"`
	Account  string    `json:"account,omitempty"`
	// InFlight counts calls in flight on the same account (proxy) or from the
	// same client (bench) when this call was dispatched, including itself.
	InFlight int `json:"in_flight"`
	// Concurrency is the configured worker count of a bench level.
	Concurrency  int     `json:"concurrency,omitempty"`
	Attempts     int     `json:"attempts,omitempty"`
	Status       int     `json:"status,omitempty"`
	Outcome      string  `json:"outcome"`
	ErrorCode    string  `json:"error_code,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	WaitMS       float64 `json:"wait_ms,omitempty"`
	DurationMS   float64 `json:"duration_ms"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	// InputTokens excludes cached input, which is counted in CacheReadTokens.
	InputTokens      int64 `json:"input_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// TokensPerSecond is output tokens divided by the time from the first
	// token to the end of the stream.
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`
}

// Fill computes the timing fields of record from an observed stream.
func (record *Record) Fill(start, end time.Time, observer *Observer) {
	record.DurationMS = milliseconds(end.Sub(start))
	if observer == nil {
		return
	}
	record.ErrorCode = observer.errorCode
	record.ErrorMessage = observer.errorMessage
	usage := observer.Usage()
	record.OutputTokens = usage.OutputTokens
	record.InputTokens = usage.InputTokens
	record.CacheReadTokens = usage.CacheReadTokens
	record.CacheWriteTokens = usage.CacheWriteTokens
	first := observer.FirstToken()
	if first.IsZero() {
		return
	}
	record.WaitMS = milliseconds(first.Sub(start))
	generation := end.Sub(first)
	if observer.Streamed() && record.OutputTokens > 1 && generation >= minGeneration {
		record.TokensPerSecond = float64(record.OutputTokens) / generation.Seconds()
	}
}

// minGeneration is the shortest generation window for which an output rate
// is reported. Short replies often arrive in one network read, which would
// produce a meaningless rate.
const minGeneration = 100 * time.Millisecond

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

// Sink receives records. Implementations must be safe for concurrent use.
type Sink interface {
	Write(Record) error
}

// Sinks writes each record to every sink, returning the first error.
type Sinks []Sink

// Write implements Sink.
func (sinks Sinks) Write(record Record) error {
	var first error
	for _, sink := range sinks {
		if err := sink.Write(record); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Writer appends records to a JSONL file. It is safe for concurrent use.
type Writer struct {
	mu     sync.Mutex
	file   *os.File
	encode *json.Encoder
}

// OpenWriter opens path for appending with owner-only permissions.
func OpenWriter(path string) (*Writer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("latency: open %s: %w", path, err)
	}
	return &Writer{file: file, encode: json.NewEncoder(file)}, nil
}

// Write appends record. A nil Writer discards it.
func (writer *Writer) Write(record Record) error {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.encode.Encode(record)
}

// Close closes the underlying file.
func (writer *Writer) Close() error {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.file.Close()
}

// ReadRecords decodes a JSONL stream, skipping blank lines.
func ReadRecords(reader io.Reader) ([]Record, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var records []Record
	line := 0
	for scanner.Scan() {
		line++
		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("latency: line %d: %w", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("latency: no records")
	}
	return records, nil
}
