package latency

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) advance(duration time.Duration) { clock.now = clock.now.Add(duration) }

func newTestObserver(clock *fakeClock) *Observer {
	return &Observer{now: func() time.Time { return clock.now }}
}

func TestObserverStreams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		events []string
		tokens int64
	}{
		{
			name: "codex responses",
			events: []string{
				`data: {"type":"response.created","response":{"usage":null}}`,
				`data: {"type":"response.output_text.delta","delta":"a"}`,
				`data: {"type":"response.completed","response":{"usage":{"output_tokens":40}}}`,
			},
			tokens: 40,
		},
		{
			name: "claude messages",
			events: []string{
				"event: message_start\n" +
					`data: {"type":"message_start","message":{"usage":{"output_tokens":1}}}`,
				"event: content_block_delta\n" +
					`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"a"}}`,
				`data: {"type":"message_delta","usage":{"output_tokens":40}}`,
				`data: {"type":"message_stop"}`,
			},
			tokens: 40,
		},
		{
			name: "chat completions",
			events: []string{
				`data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`data: {"choices":[],"usage":{"completion_tokens":40}}`,
				`data: [DONE]`,
			},
			tokens: 40,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{now: time.Unix(1000, 0)}
			start := clock.now
			observer := newTestObserver(clock)
			for index, event := range test.events {
				clock.advance(time.Second)
				payload := []byte(event + "\n\n")
				// Split each event across writes to exercise line buffering.
				_, _ = observer.Write(payload[:len(payload)/2])
				_, _ = observer.Write(payload[len(payload)/2:])
				if index == 0 && !observer.FirstToken().IsZero() {
					t.Fatal("first token set before any output delta")
				}
			}
			observer.Finish()
			if !observer.Completed() {
				t.Fatal("stream not completed")
			}
			if got := observer.OutputTokens(); got != test.tokens {
				t.Errorf("output tokens = %d, want %d", got, test.tokens)
			}
			var record Record
			record.Fill(start, clock.now, observer)
			if record.WaitMS != 2000 {
				t.Errorf("wait = %vms, want 2000", record.WaitMS)
			}
			generation := clock.now.Sub(start.Add(2 * time.Second)).Seconds()
			if want := 40 / generation; record.TokensPerSecond != want {
				t.Errorf("tokens per second = %v, want %v", record.TokensPerSecond, want)
			}
		})
	}
}

func TestObserverFailureAndJSONBody(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	failed := newTestObserver(clock)
	_, _ = failed.Write([]byte(`data: {"type":"response.failed","response":{}}` + "\n\n"))
	failed.Finish()
	if !failed.Failed() || failed.Completed() {
		t.Error("response.failed was not reported as a failure")
	}

	body := newTestObserver(clock)
	start := clock.now
	clock.advance(3 * time.Second)
	_, _ = body.Write([]byte(`{"type":"message","usage":{"output_tokens":12}}`))
	clock.advance(time.Millisecond)
	body.Finish()
	var record Record
	record.Fill(start, clock.now, body)
	if !body.Completed() || record.OutputTokens != 12 || record.WaitMS != 3000 {
		t.Errorf("JSON body record = %+v", record)
	}
	if record.TokensPerSecond != 0 {
		t.Errorf("JSON body has an output rate: %v", record.TokensPerSecond)
	}
}

func TestSummarizeBucketsByCallsInFlight(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0).UTC()
	records := []Record{
		{Time: start, Provider: "claude", InFlight: 1, Outcome: OutcomeOK, WaitMS: 1000, DurationMS: 2000, TokensPerSecond: 80},
		{Time: start, Provider: "claude", InFlight: 2, Outcome: OutcomeOK, WaitMS: 3000, DurationMS: 4000, TokensPerSecond: 60},
		{Time: start, Provider: "claude", InFlight: 9, Outcome: OutcomeTimeout, DurationMS: 9000},
		{Time: start, Provider: "codex", InFlight: 30, Outcome: OutcomeRateLimited, DurationMS: 10},
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := ReadRecords(&buffer)
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	buckets, err := ParseBuckets("3,12,24")
	if err != nil {
		t.Fatalf("parse buckets: %v", err)
	}
	rows := Summarize(
		loaded,
		func(record Record) (string, int) { return BucketLabel(buckets, record.InFlight) },
		func(record Record) string { return record.Provider },
	)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	first := rows[0]
	if first.Group != "claude" || first.Bucket != "1-3" || first.Calls != 2 || first.OK != 2 ||
		first.WaitP50 != 1000 || first.WaitP90 != 3000 || first.MeanDuration != 3000 {
		t.Errorf("first row = %+v", first)
	}
	if rows[1].Bucket != "4-12" || rows[1].Canceled != 1 {
		t.Errorf("second row = %+v", rows[1])
	}
	if rows[2].Group != "codex" || rows[2].Bucket != "25+" || rows[2].RateLimited != 1 {
		t.Errorf("third row = %+v", rows[2])
	}
	var table strings.Builder
	if err := WriteTable(&table, rows, "IN FLIGHT", false); err != nil {
		t.Fatalf("write table: %v", err)
	}
	if !strings.Contains(table.String(), "1-3") || !strings.Contains(table.String(), "100%") {
		t.Errorf("table =\n%s", table.String())
	}
}

func TestFillSkipsRateForBurstReply(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	start := clock.now
	observer := newTestObserver(clock)
	clock.advance(2 * time.Second)
	_, _ = observer.Write([]byte(`data: {"type":"content_block_delta","delta":{"text":"ok"}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":4}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"))
	clock.advance(time.Millisecond)
	observer.Finish()
	var record Record
	record.Fill(start, clock.now, observer)
	if record.WaitMS != 2000 || record.OutputTokens != 4 || record.TokensPerSecond != 0 {
		t.Errorf("record = %+v", record)
	}
}

func TestObserverUsage(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		events []string
		want   Usage
	}{
		"claude messages": {
			events: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":3000,"cache_read_input_tokens":9000,"output_tokens":1}}}`,
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
				`{"type":"message_delta","usage":{"output_tokens":250}}`,
				`{"type":"message_stop"}`,
			},
			want: Usage{InputTokens: 12, OutputTokens: 250, CacheReadTokens: 9000, CacheWriteTokens: 3000},
		},
		"codex responses": {
			events: []string{
				`{"type":"response.output_text.delta","delta":"hi"}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":5000,"input_tokens_details":{"cached_tokens":4096},"output_tokens":300}}}`,
			},
			want: Usage{InputTokens: 904, OutputTokens: 300, CacheReadTokens: 4096},
		},
		"chat completions": {
			events: []string{
				`{"choices":[{"delta":{"content":"hi"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":800,"prompt_tokens_details":{"cached_tokens":500},"completion_tokens":40}}`,
			},
			want: Usage{InputTokens: 300, OutputTokens: 40, CacheReadTokens: 500},
		},
	} {
		observer := NewObserver()
		for _, data := range test.events {
			observer.Event([]byte(data), time.Now())
		}
		if got := observer.Usage(); got != test.want {
			t.Errorf("%s: usage = %+v, want %+v", name, got, test.want)
		}
	}
}
