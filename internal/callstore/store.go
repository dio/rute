// Package callstore keeps latency records in a local SQLite database so that
// status views keep their history across restarts.
package callstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/dio/rute/internal/latency"
	// Register the pure-Go SQLite driver, which keeps builds CGO-free.
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS calls (
	time_ms           INTEGER NOT NULL,
	source            TEXT    NOT NULL,
	provider          TEXT    NOT NULL DEFAULT '',
	endpoint          TEXT    NOT NULL,
	model             TEXT    NOT NULL DEFAULT '',
	account           TEXT    NOT NULL DEFAULT '',
	in_flight         INTEGER NOT NULL,
	concurrency       INTEGER NOT NULL DEFAULT 0,
	attempts          INTEGER NOT NULL DEFAULT 0,
	status            INTEGER NOT NULL DEFAULT 0,
	outcome           TEXT    NOT NULL,
	wait_ms           REAL    NOT NULL DEFAULT 0,
	duration_ms       REAL    NOT NULL,
	output_tokens     INTEGER NOT NULL DEFAULT 0,
	tokens_per_second REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS calls_time ON calls (time_ms);
`

const columns = `time_ms, source, provider, endpoint, model, account, in_flight,
	concurrency, attempts, status, outcome, wait_ms, duration_ms, output_tokens,
	tokens_per_second`

// Store is a SQLite-backed call history. It is safe for concurrent use.
type Store struct {
	db *sql.DB
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
	return &Store{db: db}, nil
}

// Write inserts record.
func (store *Store) Write(record latency.Record) error {
	_, err := store.db.Exec(
		"INSERT INTO calls ("+columns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		record.Time.UnixMilli(),
		record.Source,
		record.Provider,
		record.Endpoint,
		record.Model,
		record.Account,
		record.InFlight,
		record.Concurrency,
		record.Attempts,
		record.Status,
		record.Outcome,
		record.WaitMS,
		record.DurationMS,
		record.OutputTokens,
		record.TokensPerSecond,
	)
	return err
}

// Since returns records that started at or after since, oldest first.
func (store *Store) Since(ctx context.Context, since time.Time) ([]latency.Record, error) {
	return store.query(ctx,
		"SELECT "+columns+" FROM calls WHERE time_ms >= ? ORDER BY time_ms",
		since.UnixMilli())
}

// Recent returns up to limit of the newest records, newest first.
func (store *Store) Recent(ctx context.Context, limit int) ([]latency.Record, error) {
	if limit < 1 {
		return nil, errors.New("callstore: limit must be positive")
	}
	return store.query(ctx,
		"SELECT "+columns+" FROM calls ORDER BY time_ms DESC LIMIT ?",
		limit)
}

// Prune deletes records that started before cutoff and returns their count.
func (store *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := store.db.ExecContext(ctx,
		"DELETE FROM calls WHERE time_ms < ?", cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Close closes the database.
func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) query(ctx context.Context, query string, args ...any) ([]latency.Record, error) {
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []latency.Record
	for rows.Next() {
		var (
			record latency.Record
			millis int64
		)
		if err := rows.Scan(
			&millis,
			&record.Source,
			&record.Provider,
			&record.Endpoint,
			&record.Model,
			&record.Account,
			&record.InFlight,
			&record.Concurrency,
			&record.Attempts,
			&record.Status,
			&record.Outcome,
			&record.WaitMS,
			&record.DurationMS,
			&record.OutputTokens,
			&record.TokensPerSecond,
		); err != nil {
			return nil, err
		}
		record.Time = time.UnixMilli(millis).UTC()
		records = append(records, record)
	}
	return records, rows.Err()
}
