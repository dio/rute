package callstore

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dio/rute/internal/latency"
)

func TestSummarySkipsCallsWithoutAnAccount(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()
	for _, record := range []latency.Record{
		{Time: now, Provider: "codex", Account: "", Model: "gpt", InFlight: 1,
			Status: 429, Outcome: latency.OutcomeRateLimited},
		{Time: now, Provider: "codex", Account: "default", Model: "gpt", InFlight: 1,
			Outcome: latency.OutcomeOK},
	} {
		if err := store.Write(record); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}

	rows, err := store.Summary(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	calls := int64(0)
	for _, row := range rows {
		if row.Account == "" {
			t.Errorf("summary kept a call with no account: %+v", row)
		}
		calls += row.Calls
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}

	usage, err := store.Usage(context.Background(), now.Add(-time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	for _, row := range usage {
		if row.Account == "" {
			t.Errorf("usage kept a call with no account: %+v", row)
		}
	}
}

func TestStoreAggregatesFlushesAndPrunes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode = %o, want 600", mode)
	}

	now := time.Now().UTC()
	old := latency.Record{Time: now.Add(-48 * time.Hour), Provider: "claude", Account: "default",
		InFlight: 1, Outcome: latency.OutcomeRateLimited, DurationMS: 300}
	calls := []latency.Record{
		old,
		{Time: now, Provider: "claude", Account: "personal", InFlight: 2, Attempts: 2,
			Outcome: latency.OutcomeOK, WaitMS: 1500, DurationMS: 4000, OutputTokens: 300, TokensPerSecond: 70},
		{Time: now, Provider: "claude", Account: "personal", InFlight: 3,
			Outcome: latency.OutcomeOK, WaitMS: 3000, DurationMS: 6000, OutputTokens: 500, TokensPerSecond: 90},
		{Time: now, Provider: "claude", Account: "personal", InFlight: 30,
			Outcome: latency.OutcomeTimeout, DurationMS: 120000},
	}
	for _, record := range calls {
		if err := store.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	// Counters are visible before they are flushed.
	summary, err := store.Summary(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	check := func(summary []*Counters) {
		t.Helper()
		if len(summary) != 2 {
			t.Fatalf("summary rows = %d, want 2", len(summary))
		}
		low, high := summary[0], summary[1]
		if low.InFlight != "2-3" || low.Calls != 2 || low.OK != 2 || low.Failovers != 1 ||
			low.OutputTokens != 800 || low.DurationMS != 10000 {
			t.Errorf("2-3 row = %+v", low)
		}
		row := low.Row()
		if row.WaitP50 < 1000 || row.WaitP50 > 2000 || row.WaitP90 < 2000 || row.WaitP90 > 4000 {
			t.Errorf("wait percentiles = %v, %v", row.WaitP50, row.WaitP90)
		}
		if row.MeanDuration != 5000 || row.TokensPerSec < 60 || row.TokensPerSec > 80 {
			t.Errorf("row = %+v", row)
		}
		if high.InFlight != "25-48" || high.Canceled != 1 || !math.IsNaN(high.Row().WaitP50) {
			t.Errorf("25-48 row = %+v", high)
		}
	}
	check(summary)

	if err := store.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// A second flush of the same minute adds to the stored row.
	if err := store.Write(calls[1]); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary[0].Calls != 3 || summary[0].Failovers != 2 {
		t.Errorf("after second flush = %+v", summary[0])
	}

	pruned, err := store.Prune(ctx, now.Add(-24*time.Hour))
	if err != nil || pruned != 1 {
		t.Fatalf("prune = %d, %v", pruned, err)
	}
	oldest, err := store.Oldest(ctx)
	if err != nil || oldest.Before(now.Add(-time.Hour)) {
		t.Fatalf("oldest = %v, %v", oldest, err)
	}

	recent := store.Recent(2)
	if len(recent) != 2 || recent[0].InFlight != 2 || recent[1].InFlight != 30 {
		t.Errorf("recent = %+v", recent)
	}

	// Counters and recent calls survive reopening.
	if err := store.Write(calls[2]); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	summary, err = reopened.Summary(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary[0].Calls != 4 {
		t.Errorf("after reopen = %+v", summary[0])
	}
	if got := reopened.Recent(10); len(got) != 6 || got[0].InFlight != 3 {
		t.Errorf("recent after reopen = %+v", got)
	}
}

func TestRecentWrapsAround(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for index := range recentSize + 5 {
		_ = store.Write(latency.Record{Time: time.Now(), Provider: "codex", InFlight: index, Outcome: latency.OutcomeOK})
	}
	recent := store.Recent(recentSize + 10)
	if len(recent) != recentSize || recent[0].InFlight != recentSize+4 || recent[recentSize-1].InFlight != 5 {
		t.Errorf("recent = %d items, first %d, last %d", len(recent), recent[0].InFlight, recent[len(recent)-1].InFlight)
	}
}

func TestHistogramQuantile(t *testing.T) {
	t.Parallel()
	single := newHistogram(waitBounds)
	single.observe(waitBounds, 2100)
	if got := single.Quantile(waitBounds, 0.5); got != 2100 {
		t.Errorf("single value p50 = %v, want 2100", got)
	}

	spread := newHistogram(waitBounds)
	for value := 1.0; value <= 10000; value++ {
		spread.observe(waitBounds, value)
	}
	for q, want := range map[float64]float64{0.5: 5000, 0.9: 9000} {
		if got := spread.Quantile(waitBounds, q); math.Abs(got-want)/want > 0.25 {
			t.Errorf("p%v = %v, want within 25%% of %v", q*100, got, want)
		}
	}

	decoded := decodeHistogram(spread.encode(), waitBounds)
	if decoded.Min != 1 || decoded.Max != 10000 || decoded.total() != 10000 {
		t.Errorf("decoded = min %v, max %v, total %d", decoded.Min, decoded.Max, decoded.total())
	}
	if !math.IsNaN(newHistogram(waitBounds).Quantile(waitBounds, 0.5)) {
		t.Error("empty histogram quantile is not NaN")
	}
}

func TestUsageGroupsByPeriodAccountAndModel(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	zone := time.FixedZone("UTC+7", 7*3600)
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, zone)
	call := func(at time.Time, account, model string, input, output, read, write int64) latency.Record {
		return latency.Record{Time: at, Provider: "claude", Account: account, Model: model, InFlight: 1,
			Outcome: latency.OutcomeOK, InputTokens: input, OutputTokens: output,
			CacheReadTokens: read, CacheWriteTokens: write}
	}
	for _, record := range []latency.Record{
		call(day.Add(9*time.Hour+5*time.Minute), "default", "opus", 10, 100, 1000, 50),
		call(day.Add(9*time.Hour+40*time.Minute), "default", "opus", 20, 200, 2000, 0),
		call(day.Add(9*time.Hour+41*time.Minute), "default", "haiku", 1, 2, 3, 4),
		call(day.Add(23*time.Hour), "personal", "opus", 5, 5, 5, 5),
	} {
		_ = store.Write(record)
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// An unflushed call in the same hour merges with stored counters.
	_ = store.Write(call(day.Add(9*time.Hour+50*time.Minute), "default", "opus", 1, 1, 1, 1))

	hourly, err := store.Usage(ctx, day, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 3 {
		t.Fatalf("hourly rows = %d, want 3: %+v", len(hourly), hourly)
	}
	opus := hourly[1]
	if !opus.Start.Equal(day.Add(9*time.Hour)) || opus.Model != "opus" || opus.Calls != 3 ||
		opus.InputTokens != 31 || opus.OutputTokens != 301 || opus.CacheReadTokens != 3001 || opus.CacheWriteTokens != 51 {
		t.Errorf("09:00 opus row = %+v", opus)
	}
	if hourly[0].Model != "haiku" || hourly[2].Account != "personal" {
		t.Errorf("hourly order = %s/%s, %s/%s", hourly[0].Account, hourly[0].Model, hourly[2].Account, hourly[2].Model)
	}

	daily, err := store.Usage(ctx, day, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 3 || !daily[0].Start.Equal(day) {
		t.Errorf("daily rows start at local midnight: %+v", daily)
	}

	totals, err := store.Usage(ctx, day, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 3 || !totals[0].Start.IsZero() {
		t.Errorf("totals = %+v", totals)
	}
}

func TestRecentPreservesErrorDiagnostics(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := latency.Record{Time: time.Now(), Provider: "codex", Account: "default", Outcome: latency.OutcomeUpstreamError, ErrorCode: "missing_tool_call", ErrorMessage: "No matching tool call"}
	if err := store.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	recent := reopened.Recent(1)
	if len(recent) != 1 || recent[0].ErrorCode != record.ErrorCode || recent[0].ErrorMessage != record.ErrorMessage {
		t.Fatalf("recent=%+v", recent)
	}
}
