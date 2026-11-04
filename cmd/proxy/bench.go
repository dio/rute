package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/bench"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/internal/pricing"
)

const defaultBenchPrompt = "Write about 300 words explaining how a hash map handles collisions."

type headerFlags []string

func (values *headerFlags) String() string { return strings.Join(*values, ", ") }

func (values *headerFlags) Set(value string) error {
	if !strings.Contains(value, ":") {
		return errors.New(`header must look like "Name: value"`)
	}
	*values = append(*values, value)
	return nil
}

func runBench(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	getenv func(string) string,
) error {
	var (
		cfg       bench.Config
		levels    string
		output    string
		apiKeyEnv string
		headers   headerFlags
	)
	flags := flag.NewFlagSet("rute bench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.BaseURL, "url", "http://"+defaultListenAddress, "server root URL")
	flags.StringVar(&cfg.API, "api", bench.APIMessages,
		"API to call: messages, chat, responses, or responses-ws")
	flags.StringVar(&cfg.Model, "model", "", "model ID (required)")
	flags.StringVar(&levels, "concurrency", "1,3,6,12", "comma-separated worker counts, run in order")
	flags.IntVar(&cfg.Rounds, "rounds", 3, "sequential calls per worker at each level")
	flags.StringVar(&cfg.Prompt, "prompt", defaultBenchPrompt, "user prompt")
	flags.IntVar(&cfg.MaxTokens, "max-tokens", 512, "output token limit (messages and chat)")
	flags.StringVar(&cfg.Effort, "effort", "", "reasoning effort (chat and responses)")
	flags.DurationVar(&cfg.Timeout, "timeout", 3*time.Minute, "per-call time limit")
	flags.StringVar(&output, "output", "", "append JSONL records to this file")
	flags.StringVar(&apiKeyEnv, "api-key-env", "",
		"environment variable holding an API key, for benchmarking a provider API directly")
	flags.Var(&headers, "header", `extra request header "Name: value" (repeatable)`)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	parsed, err := parseLevels(levels)
	if err != nil {
		return err
	}
	cfg.Levels = parsed
	cfg.Header = http.Header{}
	for _, header := range headers {
		name, value, _ := strings.Cut(header, ":")
		cfg.Header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	if apiKeyEnv != "" {
		key := strings.TrimSpace(getenv(apiKeyEnv))
		if key == "" {
			return fmt.Errorf("%s is empty", apiKeyEnv)
		}
		if cfg.API == bench.APIMessages {
			cfg.Header.Set("X-Api-Key", key)
		} else {
			cfg.Header.Set("Authorization", "Bearer "+key)
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	var writer *latency.Writer
	if output != "" {
		writer, err = latency.OpenWriter(output)
		if err != nil {
			return err
		}
		defer func() { _ = writer.Close() }()
	}
	cfg.Record = func(record latency.Record) {
		_ = writer.Write(record)
		_, _ = fmt.Fprintf(stderr, "c=%-3d %-14s wait=%6.1fs tok/s=%5.0f dur=%6.1fs\n",
			record.Concurrency, record.Outcome, record.WaitMS/1000,
			record.TokensPerSecond, record.DurationMS/1000)
	}
	records, runErr := bench.Run(ctx, cfg)
	if len(records) > 0 {
		rows := latency.Summarize(
			records,
			func(record latency.Record) (string, int) {
				return strconv.Itoa(record.Concurrency), record.Concurrency
			},
			func(latency.Record) string { return cfg.API + " " + cfg.Model },
		)
		if _, err := fmt.Fprintln(stdout); err != nil {
			return err
		}
		if err := latency.WriteTable(stdout, rows, "WORKERS", true); err != nil {
			return err
		}
	}
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	return runErr
}

func parseLevels(value string) ([]int, error) {
	var levels []int
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		level, err := strconv.Atoi(part)
		if err != nil || level < 1 {
			return nil, fmt.Errorf("invalid concurrency level %q", part)
		}
		if len(levels) > 0 && level <= levels[len(levels)-1] {
			return nil, errors.New("concurrency levels must be ascending")
		}
		levels = append(levels, level)
	}
	if len(levels) == 0 {
		return nil, errors.New("at least one concurrency level is required")
	}
	return levels, nil
}

func runStats(args []string, stdout, stderr io.Writer) error {
	var (
		buckets  string
		by       string
		database string
		window   time.Duration
		usage    string
		prices   string
	)
	flags := flag.NewFlagSet("rute stats", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&buckets, "buckets", "1,3,6,12,24,48",
		"ascending upper bounds of the calls-in-flight groups")
	flags.StringVar(&by, "by", "provider",
		"comma-separated grouping fields: provider, account, model, endpoint, source")
	flags.StringVar(&database, "metrics-db", "",
		"call history database to read when no JSONL files are given (default: metrics.db in the state directory)")
	flags.DurationVar(&window, "window", 24*time.Hour, "how far back to read from the database")
	flags.StringVar(&usage, "usage", "",
		"print token usage and estimated cost from the database by hour, day, or total")
	flags.StringVar(&prices, "prices", "",
		"JSON price list for -usage (default: prices.json in the state directory)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	bounds, err := latency.ParseBuckets(buckets)
	if err != nil {
		return err
	}
	fields := strings.Split(by, ",")
	for _, field := range fields {
		switch strings.TrimSpace(field) {
		case "provider", "account", "model", "endpoint", "source":
		default:
			return fmt.Errorf("unsupported grouping field %q", field)
		}
	}
	var records []latency.Record
	if flags.NArg() == 0 || usage != "" {
		if flags.NArg() != 0 {
			return errors.New("-usage reads the database; do not pass JSONL files")
		}
		if database == "" || prices == "" {
			dir, err := accountstore.DefaultStateDir()
			if err != nil {
				return err
			}
			if database == "" {
				database = filepath.Join(dir, "metrics.db")
			}
			if prices == "" {
				prices = filepath.Join(dir, "prices.json")
			}
		}
		if _, err := os.Stat(database); err != nil {
			return fmt.Errorf("no JSONL files given and no call history at %s", database)
		}
		calls, err := callstore.Open(database)
		if err != nil {
			return err
		}
		if usage != "" {
			defer func() { _ = calls.Close() }()
			return writeUsage(stdout, calls, pricing.NewFile(prices), time.Now().Add(-window), usage)
		}
		counters, err := calls.Summary(context.Background(), time.Now().Add(-window))
		_ = calls.Close()
		if err != nil {
			return err
		}
		if len(counters) == 0 {
			return fmt.Errorf("no calls in %s in the last %s", database, window)
		}
		rows := make([]latency.Row, 0, len(counters))
		for _, item := range counters {
			rows = append(rows, item.Row())
		}
		return latency.WriteTable(stdout, rows, "IN FLIGHT", false)
	}
	for _, path := range flags.Args() {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		loaded, err := latency.ReadRecords(file)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		records = append(records, loaded...)
	}
	inFlightBucket := func(record latency.Record) (string, int) {
		return latency.BucketLabel(bounds, max(1, record.InFlight))
	}
	rows := latency.Summarize(records, inFlightBucket, func(record latency.Record) string {
		parts := make([]string, 0, len(fields))
		for _, field := range fields {
			var value string
			switch strings.TrimSpace(field) {
			case "provider":
				value = record.Provider
			case "account":
				value = record.Account
			case "model":
				value = record.Model
			case "endpoint":
				value = record.Endpoint
			case "source":
				value = record.Source
			}
			if value == "" {
				value = "-"
			}
			parts = append(parts, value)
		}
		return strings.Join(parts, "/")
	})
	return latency.WriteTable(stdout, rows, "IN FLIGHT", false)
}

// writeUsage prints token usage per period, account, and model with the
// estimated API-equivalent cost of each row.
func writeUsage(stdout io.Writer, calls *callstore.Store, prices *pricing.File, since time.Time, by string) error {
	steps := map[string]time.Duration{"hour": time.Hour, "day": 24 * time.Hour, "total": 0}
	step, ok := steps[by]
	if !ok {
		return errors.New("-usage must be hour, day, or total")
	}
	table, err := prices.Table()
	if err != nil {
		return err
	}
	rows, err := calls.Usage(context.Background(), since.In(time.Local), step)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(writer, "PERIOD\tACCOUNT\tMODEL\tCALLS\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tEST. COST\t")
	var total float64
	for _, row := range rows {
		period := "total"
		if !row.Start.IsZero() {
			period = row.Start.Local().Format("2006-01-02 15:04")
		}
		cost := "-"
		if rate, ok := table.Lookup(row.Model); ok {
			value := rate.Cost(pricing.Tokens{Input: row.InputTokens, Output: row.OutputTokens,
				CacheRead: row.CacheReadTokens, CacheWrite: row.CacheWriteTokens})
			total += value
			cost = fmt.Sprintf("$%.2f", value)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s/%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t\n", period, row.Provider, row.Account,
			row.Model, row.Calls, row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens, cost)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "estimated API-equivalent cost of priced models: $%.2f (from %s)\n", total, prices.Path())
	return err
}
