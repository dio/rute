package latency

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecordStreamErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data string }{
		{"error event", `{"type":"error","error":{"code":"previous_response_not_found","message":"Missing response"}}`},
		{"failed response", `{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"Missing response"}}}`},
		{"nested error", `{"response":{"error":{"code":"previous_response_not_found","message":"Missing response"}}}`},
		{"flat error", `{"type":"error","code":"previous_response_not_found","message":"Missing response"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := NewObserver()
			_, _ = observer.Write([]byte("data: " + tc.data + "\n\n"))
			observer.Finish()
			if !observer.Failed() {
				t.Error("not marked failed")
			}
			var record Record
			record.Fill(time.Now(), time.Now(), observer)
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"error_code":"previous_response_not_found"`) || !strings.Contains(string(data), `"error_message":"Missing response"`) {
				t.Errorf("missing error diagnostics: %s", data)
			}
		})
	}
}

func TestErrorDiagnosticsAreBounded(t *testing.T) {
	t.Parallel()
	observer := NewObserver()
	data, _ := json.Marshal(map[string]any{"error": map[string]string{"code": strings.Repeat("c", 300), "message": strings.Repeat("界", 1000)}})
	observer.Event(data, time.Now())
	observer.Event([]byte(`{"error":{"code":"later","message":"later"}}`), time.Now())
	var record Record
	record.Fill(time.Now(), time.Now(), observer)
	if len(record.ErrorCode) != 256 || len(record.ErrorMessage) > 2048 || len(record.ErrorMessage) < 2045 {
		t.Fatalf("unexpected diagnostics lengths: %d, %d", len(record.ErrorCode), len(record.ErrorMessage))
	}
}
