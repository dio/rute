package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLookupAndCost(t *testing.T) {
	t.Parallel()
	table, err := Parse([]byte(`{"models": {
		"model-a": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5},
		"model-*": {"input": 1, "output": 1},
		"model-b-*": {"input": 4, "output": 4}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]float64{"model-a": 2, "model-b-mini": 4, "model-c": 1} {
		rate, ok := table.Lookup(model)
		if !ok || rate.Input != want {
			t.Errorf("%s: rate = %+v, %v; want input %v", model, rate, ok, want)
		}
	}
	if _, ok := table.Lookup("other"); ok {
		t.Error("unlisted model has a rate")
	}
	rate, _ := table.Lookup("model-a")
	cost := rate.Cost(Tokens{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 400_000})
	if math.Abs(cost-(2+1+0.4+1)) > 1e-9 {
		t.Errorf("cost = %v, want 4.4", cost)
	}
	if _, err := Parse([]byte(`{"models":{"x":{"input":-1}}}`)); err == nil {
		t.Error("negative rate accepted")
	}
}

func TestFileReloads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "prices.json")
	file := NewFile(path)
	if table, err := file.Table(); err != nil || len(table.Models) != 0 {
		t.Fatalf("missing file = %+v, %v", table, err)
	}
	if err := os.WriteFile(path, []byte(`{"models":{"m":{"input":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if table, err := file.Table(); err != nil || table.Models["m"].Input != 1 {
		t.Fatalf("after write = %+v, %v", table, err)
	}
	if err := os.WriteFile(path, []byte(`{"models":{"m":{"input":3}}, "x":`), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(path, later, later)
	if _, err := file.Table(); err == nil {
		t.Error("invalid file did not report an error")
	}
}
