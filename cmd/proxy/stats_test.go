package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/latency"
)

func TestStatsUsageReadsDatabaseWithPrices(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "metrics.db")
	calls, err := callstore.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	_ = calls.Write(latency.Record{Time: time.Now(), Provider: "codex", Account: "default", Model: "gpt-test",
		Outcome: latency.OutcomeOK, InputTokens: 500_000, OutputTokens: 100_000})
	if err := calls.Close(); err != nil {
		t.Fatal(err)
	}
	prices := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(prices, []byte(`{"models":{"gpt-test":{"input":2,"output":10}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runStats([]string{"-metrics-db", database, "-prices", prices, "-usage", "total"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("stats: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"codex/default", "gpt-test", "500000", "100000", "$2.00", "priced models: $2.00"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q:\n%s", want, stdout.String())
		}
	}
	if err := runStats([]string{"-metrics-db", database, "-usage", "week"}, &stdout, &stderr); err == nil {
		t.Error("invalid -usage accepted")
	}
}
