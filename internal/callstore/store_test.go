package callstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dio/rute/internal/latency"
)

func TestStoreWritesQueriesAndPrunes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode = %o, want 600", mode)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	records := []latency.Record{
		{Time: now.Add(-48 * time.Hour), Source: "proxy", Provider: "claude", Endpoint: "messages",
			Account: "default", InFlight: 1, Outcome: latency.OutcomeRateLimited, Status: 429, DurationMS: 300},
		{Time: now.Add(-time.Hour), Source: "proxy", Provider: "claude", Endpoint: "messages",
			Model: "claude-sonnet-5-5", Account: "personal", InFlight: 2, Attempts: 2, Status: 200,
			Outcome: latency.OutcomeOK, WaitMS: 1200.5, DurationMS: 4000, OutputTokens: 300, TokensPerSecond: 107.2},
		{Time: now, Source: "proxy", Provider: "codex", Endpoint: "responses_ws",
			Account: "codex-a", InFlight: 1, Outcome: latency.OutcomeCanceled, DurationMS: 60000},
	}
	for _, record := range records {
		if err := store.Write(record); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	ctx := context.Background()
	since, err := store.Since(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(since) != 2 || since[0] != records[1] || since[1] != records[2] {
		t.Fatalf("since = %+v", since)
	}
	recent, err := store.Recent(ctx, 2)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Account != "codex-a" || recent[1].Account != "personal" {
		t.Fatalf("recent = %+v", recent)
	}
	pruned, err := store.Prune(ctx, now.Add(-24*time.Hour))
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}

	// History survives reopening.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	all, err := reopened.Since(ctx, time.Time{})
	if err != nil || len(all) != 2 {
		t.Fatalf("after reopen = %d records, %v", len(all), err)
	}
}
