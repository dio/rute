package idle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTrackerCountsWorkAndIdleTime(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	tracker := New()
	tracker.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	tracker.last = now
	advance := func(duration time.Duration) { mu.Lock(); now = now.Add(duration); mu.Unlock() }

	advance(time.Minute)
	if got := tracker.IdleFor(); got != time.Minute {
		t.Fatalf("idle = %v, want 1m", got)
	}
	end := tracker.Begin()
	advance(time.Hour)
	if got := tracker.IdleFor(); got != 0 {
		t.Fatalf("idle while active = %v", got)
	}
	end()
	end() // A second call does nothing.
	if tracker.Active() != 0 {
		t.Fatalf("active = %d after end", tracker.Active())
	}
	advance(2 * time.Minute)
	if got := tracker.IdleFor(); got != 2*time.Minute {
		t.Fatalf("idle after work = %v, want 2m", got)
	}

	handler := tracker.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		if tracker.Active() != 1 {
			t.Errorf("active during request = %d", tracker.Active())
		}
		advance(time.Second)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := tracker.IdleFor(); got != 0 {
		t.Errorf("idle right after request = %v", got)
	}

	advance(time.Hour)
	if !tracker.Wait(context.Background(), time.Hour, time.Millisecond) {
		t.Error("Wait did not report idle")
	}
	tracker.Begin()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if tracker.Wait(ctx, time.Nanosecond, time.Millisecond) {
		t.Error("Wait reported idle while work is active")
	}

	tracker.Stop()
	tracker.Stop()
	select {
	case <-tracker.Stopping():
	default:
		t.Error("Stopping is not closed after Stop")
	}
}
