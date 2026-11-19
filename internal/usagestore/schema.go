package usagestore

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is stored in PRAGMA user_version.
const schemaVersion = 1

// schemaV1 keeps one row per upstream request and hourly totals per
// provider, credential, model and client API key. Token columns hold the
// canonical, non-overlapping buckets of usage.TokenBreakdown.
var schemaV1 = []string{
	`CREATE TABLE requests (
		id                  INTEGER PRIMARY KEY,
		request_id          TEXT    NOT NULL UNIQUE,
		ts_ms               INTEGER NOT NULL,
		provider            TEXT    NOT NULL,
		auth_index          TEXT    NOT NULL,
		model               TEXT    NOT NULL,
		alias               TEXT    NOT NULL,
		api_key_fp          TEXT    NOT NULL,
		session_id          TEXT    NOT NULL,
		parent_session_id   TEXT    NOT NULL,
		stream              INTEGER NOT NULL,
		failed              INTEGER NOT NULL,
		status_code         INTEGER NOT NULL,
		latency_ms          INTEGER,
		ttft_ms             INTEGER,
		input_tokens        INTEGER NOT NULL,
		cache_read_tokens   INTEGER NOT NULL,
		cache_write_tokens  INTEGER NOT NULL,
		output_tokens       INTEGER NOT NULL,
		reasoning_tokens    INTEGER NOT NULL,
		unclassified_tokens INTEGER NOT NULL
	)`,
	`CREATE INDEX requests_ts ON requests (ts_ms)`,
	`CREATE INDEX requests_auth_ts ON requests (auth_index, ts_ms)`,
	`CREATE INDEX requests_session ON requests (session_id, ts_ms) WHERE session_id <> ''`,
	`CREATE TABLE usage_hourly (
		hour_ms             INTEGER NOT NULL,
		provider            TEXT    NOT NULL,
		auth_index          TEXT    NOT NULL,
		model               TEXT    NOT NULL,
		api_key_fp          TEXT    NOT NULL,
		requests            INTEGER NOT NULL,
		failed              INTEGER NOT NULL,
		input_tokens        INTEGER NOT NULL,
		cache_read_tokens   INTEGER NOT NULL,
		cache_write_tokens  INTEGER NOT NULL,
		output_tokens       INTEGER NOT NULL,
		reasoning_tokens    INTEGER NOT NULL,
		unclassified_tokens INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, provider, auth_index, model, api_key_fp)
	)`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf("schema version %d is newer than the supported version %d", version, schemaVersion)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range schemaV1 {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply schema version %d: %w", schemaVersion, err)
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
