// Package callstore keeps per-minute call counters in a local SQLite database
// so status views keep their history across restarts. It aggregates calls as
// they arrive and never stores individual calls.
package callstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dio/rute/internal/latency"
	// Register the pure-Go SQLite driver, which keeps builds CGO-free.
	_ "modernc.org/sqlite"
)

const (
	flushInterval = 10 * time.Second
	pruneInterval = time.Hour
	// recentSize is how many individual calls Recent keeps in memory.
	recentSize = 1000
)

// InFlightBuckets are the upper bounds of the calls-in-flight ranges.
var InFlightBuckets = []int{1, 3, 6, 12, 24, 48}

// Histogram upper bounds. A final bucket holds larger values.
var (
	waitBounds = []float64{250, 500, 1000, 2000, 4000, 8000, 16000, 32000, 64000, 128000}
	rateBounds = []float64{5, 10, 20, 40, 60, 80, 100, 150, 200, 300}
)

const schema = `
CREATE TABLE IF NOT EXISTS minutes (
	minute        INTEGER NOT NULL,
	provider      TEXT    NOT NULL,
	account       TEXT    NOT NULL,
	model         TEXT    NOT NULL,
	bucket        INTEGER NOT NULL,
	calls         INTEGER NOT NULL,
	ok            INTEGER NOT NULL,
	rate_limited  INTEGER NOT NULL,
	canceled      INTEGER NOT NULL,
	errors        INTEGER NOT NULL,
	failovers     INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	input_tokens  INTEGER NOT NULL,
	cache_read    INTEGER NOT NULL,
	cache_write   INTEGER NOT NULL,
	duration_ms   REAL    NOT NULL,
	wait_hist     TEXT    NOT NULL,
	rate_hist     TEXT    NOT NULL,
	PRIMARY KEY (minute, provider, account, model, bucket)
) WITHOUT ROWID;
`

// Counters aggregates calls for one provider, account, model, and
// calls-in-flight range. Fields a query merges across are left empty.
type Counters struct {
	// Start is the start of the period the counters cover, when grouped by
	// time.
	Start        time.Time `json:"start,omitzero"`
	Provider     string    `json:"provider"`
	Account      string    `json:"account"`
	Model        string    `json:"model,omitempty"`
	InFlight     string    `json:"in_flight,omitempty"`
	Bucket       int       `json:"-"`
	Calls        int64     `json:"calls"`
	OK           int64     `json:"ok"`
	RateLimited  int64     `json:"rate_limited"`
	Canceled     int64     `json:"canceled"`
	Errors       int64     `json:"errors"`
	Failovers    int64     `json:"failovers"`
	OutputTokens int64     `json:"output_tokens"`
	// InputTokens excludes cached input.
	InputTokens      int64      `json:"input_tokens"`
	CacheReadTokens  int64      `json:"cache_read_tokens"`
	CacheWriteTokens int64      `json:"cache_write_tokens"`
	DurationMS       float64    `json:"-"`
	Wait             *Histogram `json:"-"`
	Rate             *Histogram `json:"-"`
}

type key struct {
	minute   int64
	provider string
	account  string
	model    string
	bucket   int
}

// bucketLabel names a calls-in-flight range, such as "4-6" or "49+".
func bucketLabel(bucket int) string {
	value := InFlightBuckets[len(InFlightBuckets)-1] + 1
	if bucket < len(InFlightBuckets) {
		value = InFlightBuckets[bucket]
	}
	label, _ := latency.BucketLabel(InFlightBuckets, value)
	return label
}

func newCounters(id key) *Counters {
	return &Counters{
		Provider: id.provider,
		Account:  id.account,
		Model:    id.model,
		Bucket:   id.bucket,
		InFlight: bucketLabel(id.bucket),
		Wait:     newHistogram(waitBounds),
		Rate:     newHistogram(rateBounds),
	}
}

func (counters *Counters) add(record latency.Record) {
	counters.Calls++
	switch record.Outcome {
	case latency.OutcomeOK:
		counters.OK++
	case latency.OutcomeRateLimited:
		counters.RateLimited++
	case latency.OutcomeCanceled, latency.OutcomeTimeout:
		counters.Canceled++
	default:
		counters.Errors++
	}
	if record.Attempts > 1 {
		counters.Failovers++
	}
	counters.OutputTokens += record.OutputTokens
	counters.InputTokens += record.InputTokens
	counters.CacheReadTokens += record.CacheReadTokens
	counters.CacheWriteTokens += record.CacheWriteTokens
	counters.DurationMS += record.DurationMS
	if record.WaitMS > 0 {
		counters.Wait.observe(waitBounds, record.WaitMS)
	}
	if record.TokensPerSecond > 0 {
		counters.Rate.observe(rateBounds, record.TokensPerSecond)
	}
}

func (counters *Counters) merge(other *Counters) {
	counters.Calls += other.Calls
	counters.OK += other.OK
	counters.RateLimited += other.RateLimited
	counters.Canceled += other.Canceled
	counters.Errors += other.Errors
	counters.Failovers += other.Failovers
	counters.OutputTokens += other.OutputTokens
	counters.InputTokens += other.InputTokens
	counters.CacheReadTokens += other.CacheReadTokens
	counters.CacheWriteTokens += other.CacheWriteTokens
	counters.DurationMS += other.DurationMS
	counters.Wait.merge(other.Wait)
	counters.Rate.merge(other.Rate)
}

// ByAccount merges counters across calls-in-flight ranges, keyed by
// "provider/account".
func ByAccount(counters []*Counters) map[string]*Counters {
	merged := make(map[string]*Counters)
	for _, item := range counters {
		id := item.Provider + "/" + item.Account
		target := merged[id]
		if target == nil {
			target = newCounters(key{provider: item.Provider, account: item.Account})
			target.InFlight = "all"
			merged[id] = target
		}
		target.merge(item)
	}
	return merged
}

// Row converts the counters to a summary row with estimated percentiles.
func (counters *Counters) Row() latency.Row {
	row := latency.Row{
		Group:        counters.Provider + "/" + counters.Account,
		Bucket:       counters.InFlight,
		Calls:        int(counters.Calls),
		OK:           int(counters.OK),
		RateLimited:  int(counters.RateLimited),
		Canceled:     int(counters.Canceled),
		Errors:       int(counters.Errors),
		WaitP50:      counters.Wait.Quantile(waitBounds, 0.5),
		WaitP90:      counters.Wait.Quantile(waitBounds, 0.9),
		TokensPerSec: counters.Rate.Quantile(rateBounds, 0.5),
		MeanDuration: math.NaN(),
		OutputTokens: counters.OutputTokens,
	}
	if counters.Calls > 0 {
		row.MeanDuration = counters.DurationMS / float64(counters.Calls)
	}
	return row
}

// Histogram counts values in fixed buckets and keeps the smallest and largest
// value, which bound its estimates.
type Histogram struct {
	Counts []int64
	Min    float64
	Max    float64
}

func newHistogram(bounds []float64) *Histogram {
	return &Histogram{Counts: make([]int64, len(bounds)+1)}
}

func (histogram *Histogram) total() int64 {
	var total int64
	for _, count := range histogram.Counts {
		total += count
	}
	return total
}

func (histogram *Histogram) observe(bounds []float64, value float64) {
	if histogram.total() == 0 || value < histogram.Min {
		histogram.Min = value
	}
	if histogram.total() == 0 || value > histogram.Max {
		histogram.Max = value
	}
	index, _ := slices.BinarySearch(bounds, value)
	histogram.Counts[index]++
}

func (histogram *Histogram) merge(other *Histogram) {
	if other.total() == 0 {
		return
	}
	if histogram.total() == 0 {
		histogram.Min, histogram.Max = other.Min, other.Max
	} else {
		histogram.Min = min(histogram.Min, other.Min)
		histogram.Max = max(histogram.Max, other.Max)
	}
	for index := range min(len(histogram.Counts), len(other.Counts)) {
		histogram.Counts[index] += other.Counts[index]
	}
}

// Quantile estimates the q quantile by linear interpolation within the bucket
// that contains it, limited to the observed minimum and maximum. It returns
// NaN when the histogram is empty.
func (histogram *Histogram) Quantile(bounds []float64, q float64) float64 {
	total := histogram.total()
	if total == 0 {
		return math.NaN()
	}
	target := q * float64(total)
	var seen float64
	estimate := histogram.Max
	for index, count := range histogram.Counts {
		if count == 0 {
			continue
		}
		if seen+float64(count) >= target {
			lower, upper := histogram.Min, histogram.Max
			if index > 0 {
				lower = max(lower, bounds[index-1])
			}
			if index < len(bounds) {
				upper = min(upper, bounds[index])
			}
			fraction := (target - seen) / float64(count)
			estimate = lower + fraction*(upper-lower)
			break
		}
		seen += float64(count)
	}
	return min(max(estimate, histogram.Min), histogram.Max)
}

// encode stores the histogram as "min,max;count,count,...".
func (histogram *Histogram) encode() string {
	parts := make([]string, len(histogram.Counts))
	for index, count := range histogram.Counts {
		parts[index] = strconv.FormatInt(count, 10)
	}
	return strconv.FormatFloat(histogram.Min, 'g', -1, 64) + "," +
		strconv.FormatFloat(histogram.Max, 'g', -1, 64) + ";" + strings.Join(parts, ",")
}

func decodeHistogram(value string, bounds []float64) *Histogram {
	histogram := newHistogram(bounds)
	limits, counts, _ := strings.Cut(value, ";")
	low, high, _ := strings.Cut(limits, ",")
	histogram.Min, _ = strconv.ParseFloat(low, 64)
	histogram.Max, _ = strconv.ParseFloat(high, 64)
	for index, part := range strings.Split(counts, ",") {
		if index >= len(histogram.Counts) {
			break
		}
		histogram.Counts[index], _ = strconv.ParseInt(part, 10, 64)
	}
	return histogram
}

// Store aggregates calls in memory and writes per-minute counters to SQLite.
// It is safe for concurrent use.
type Store struct {
	db *sql.DB

	mu      sync.Mutex
	pending map[key]*Counters
	recent  []latency.Record
	next    int
}

// Open opens or creates the database at path with owner-only permissions.
func Open(path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("callstore: create %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("callstore: open %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("callstore: create schema: %w", err)
	}
	return &Store{db: db, pending: make(map[key]*Counters)}, nil
}

// Write counts record. It does no disk I/O.
func (store *Store) Write(record latency.Record) error {
	_, bucket := latency.BucketLabel(InFlightBuckets, max(1, record.InFlight))
	id := key{
		minute:   record.Time.Truncate(time.Minute).Unix(),
		provider: record.Provider,
		account:  record.Account,
		model:    record.Model,
		bucket:   bucket,
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	counters := store.pending[id]
	if counters == nil {
		counters = newCounters(id)
		store.pending[id] = counters
	}
	counters.add(record)
	if len(store.recent) < recentSize {
		store.recent = append(store.recent, record)
	} else {
		store.recent[store.next] = record
	}
	store.next = (store.next + 1) % recentSize
	return nil
}

// Recent returns up to limit of the newest calls since the process started,
// newest first. Individual calls are kept only in memory.
func (store *Store) Recent(limit int) []latency.Record {
	store.mu.Lock()
	defer store.mu.Unlock()
	records := make([]latency.Record, 0, min(limit, len(store.recent)))
	for offset := 1; offset <= len(store.recent) && len(records) < limit; offset++ {
		index := (store.next - offset + recentSize) % recentSize
		if len(store.recent) < recentSize {
			index = len(store.recent) - offset
		}
		records = append(records, store.recent[index])
	}
	return records
}

// Flush writes pending counters to the database.
func (store *Store) Flush(ctx context.Context) error {
	store.mu.Lock()
	pending := store.pending
	store.pending = make(map[key]*Counters)
	store.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	err := store.write(ctx, pending)
	if err != nil {
		// Keep the counters for the next flush.
		store.mu.Lock()
		for id, counters := range pending {
			if existing := store.pending[id]; existing != nil {
				counters.merge(existing)
			}
			store.pending[id] = counters
		}
		store.mu.Unlock()
	}
	return err
}

const columns = `minute, provider, account, model, bucket, calls, ok, rate_limited, canceled,
	errors, failovers, output_tokens, input_tokens, cache_read, cache_write, duration_ms,
	wait_hist, rate_hist`

func (store *Store) write(ctx context.Context, pending map[key]*Counters) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for id, counters := range pending {
		existing, _, err := scanCounters(tx.QueryRowContext(ctx,
			"SELECT "+columns+` FROM minutes
			WHERE minute = ? AND provider = ? AND account = ? AND model = ? AND bucket = ?`,
			id.minute, id.provider, id.account, id.model, id.bucket))
		merged := newCounters(id)
		merged.merge(counters)
		if err == nil {
			merged.merge(existing)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR REPLACE INTO minutes ("+columns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id.minute, id.provider, id.account, id.model, id.bucket, merged.Calls, merged.OK,
			merged.RateLimited, merged.Canceled, merged.Errors, merged.Failovers,
			merged.OutputTokens, merged.InputTokens, merged.CacheReadTokens, merged.CacheWriteTokens,
			merged.DurationMS, merged.Wait.encode(), merged.Rate.encode(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanCounters(row scanner) (*Counters, key, error) {
	var (
		id         key
		wait, rate string
	)
	counters := &Counters{}
	if err := row.Scan(&id.minute, &id.provider, &id.account, &id.model, &id.bucket,
		&counters.Calls, &counters.OK, &counters.RateLimited, &counters.Canceled,
		&counters.Errors, &counters.Failovers, &counters.OutputTokens, &counters.InputTokens,
		&counters.CacheReadTokens, &counters.CacheWriteTokens, &counters.DurationMS,
		&wait, &rate); err != nil {
		return nil, key{}, err
	}
	result := newCounters(id)
	counters.Wait = decodeHistogram(wait, waitBounds)
	counters.Rate = decodeHistogram(rate, rateBounds)
	result.merge(counters)
	return result, id, nil
}

// aggregate merges stored and pending counters for minutes at or after since
// into groups chosen by group, which maps a minute's key to its group key.
func (store *Store) aggregate(ctx context.Context, since time.Time, group func(key) key) ([]*Counters, error) {
	start := since.Truncate(time.Minute).Unix()
	merged := map[key]*Counters{}
	add := func(id key, counters *Counters) {
		target := group(id)
		if merged[target] == nil {
			merged[target] = newCounters(target)
			if target.minute != 0 {
				merged[target].Start = time.Unix(target.minute, 0).UTC()
			}
		}
		merged[target].merge(counters)
	}
	rows, err := store.db.QueryContext(ctx, "SELECT "+columns+" FROM minutes WHERE minute >= ?", start)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		counters, id, err := scanCounters(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		add(id, counters)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	for id, counters := range store.pending {
		if id.minute >= start {
			add(id, counters)
		}
	}
	store.mu.Unlock()

	result := make([]*Counters, 0, len(merged))
	for _, counters := range merged {
		result = append(result, counters)
	}
	slices.SortFunc(result, func(left, right *Counters) int {
		if compared := left.Start.Compare(right.Start); compared != 0 {
			return compared
		}
		for _, pair := range [][2]string{
			{left.Provider, right.Provider}, {left.Account, right.Account}, {left.Model, right.Model},
		} {
			if compared := strings.Compare(pair[0], pair[1]); compared != 0 {
				return compared
			}
		}
		return left.Bucket - right.Bucket
	})
	return result, nil
}

// Summary returns counters merged by provider, account, and calls-in-flight
// range for calls that started at or after since, including counters not yet
// flushed. Rows are ordered by provider, account, and range.
func (store *Store) Summary(ctx context.Context, since time.Time) ([]*Counters, error) {
	return store.aggregate(ctx, since, func(id key) key {
		return key{provider: id.provider, account: id.account, bucket: id.bucket}
	})
}

// Usage returns counters merged by period, provider, account, and model for
// calls that started at or after since. Periods are step long and aligned to
// step in the location of since, so a 24h step starts at local midnight.
// A zero step returns one period per provider, account, and model.
func (store *Store) Usage(ctx context.Context, since time.Time, step time.Duration) ([]*Counters, error) {
	_, offset := since.Zone()
	rows, err := store.aggregate(ctx, since, func(id key) key {
		grouped := key{minute: 0, provider: id.provider, account: id.account, model: id.model}
		if step > 0 {
			seconds := int64(step.Seconds())
			local := id.minute + int64(offset)
			grouped.minute = local - local%seconds - int64(offset)
		}
		return grouped
	})
	for _, row := range rows {
		row.InFlight = ""
	}
	return rows, err
}

// Oldest returns the start of the oldest stored minute, or the zero time when
// the store is empty.
func (store *Store) Oldest(ctx context.Context) (time.Time, error) {
	var minute sql.NullInt64
	if err := store.db.QueryRowContext(ctx, "SELECT MIN(minute) FROM minutes").Scan(&minute); err != nil {
		return time.Time{}, err
	}
	if minute.Valid {
		return time.Unix(minute.Int64, 0).UTC(), nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var oldest int64
	for id := range store.pending {
		if oldest == 0 || id.minute < oldest {
			oldest = id.minute
		}
	}
	if oldest == 0 {
		return time.Time{}, nil
	}
	return time.Unix(oldest, 0).UTC(), nil
}

// Prune deletes counters for minutes before cutoff and returns how many rows
// it deleted.
func (store *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := store.db.ExecContext(ctx, "DELETE FROM minutes WHERE minute < ?", cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Run flushes counters every ten seconds and deletes counters older than
// retention every hour, until ctx is cancelled. It reports errors to report.
func (store *Store) Run(ctx context.Context, retention time.Duration, report func(error)) {
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneInterval)
	defer prune.Stop()
	if _, err := store.Prune(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
		report(err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			if err := store.Flush(ctx); err != nil && ctx.Err() == nil {
				report(err)
			}
		case <-prune.C:
			if _, err := store.Prune(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
				report(err)
			}
		}
	}
}

// Close flushes pending counters and closes the database.
func (store *Store) Close() error {
	flushErr := store.Flush(context.Background())
	return errors.Join(flushErr, store.db.Close())
}
