// Package usagestore persists usage records in SQLite and answers usage
// history queries for the management API.
package usagestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	inputBuffer       = 4096
	maxBatchRows      = 200
	flushInterval     = time.Second
	retentionInterval = time.Hour
	dropLogInterval   = time.Minute
)

// Retention is how long the store keeps per-request rows and hourly totals.
type Retention struct {
	RequestDays int
	RollupDays  int
}

func (r Retention) validate() error {
	if r.RequestDays < 1 || r.RollupDays < 1 {
		return fmt.Errorf("usage store: retention must be at least one day, got %d request days and %d rollup days", r.RequestDays, r.RollupDays)
	}
	return nil
}

// Options configures Open.
type Options struct {
	// Path is the SQLite database file. Its directory must exist.
	Path      string
	Retention Retention
}

// Store is a usage.Plugin that writes every record to SQLite on one writer
// goroutine, so a slow disk never blocks the usage dispatcher.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// mu guards closed and sending on in; Close takes it exclusively.
	mu     sync.RWMutex
	closed bool
	in     chan row
	done   chan struct{}

	retention atomic.Pointer[Retention]

	dropped     atomic.Uint64
	writeErrors atomic.Uint64
	lastDropLog atomic.Int64
}

// Open opens or creates the database at opts.Path, applies the schema and
// starts the writer.
func Open(ctx context.Context, opts Options) (*Store, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("usage store: database path is empty")
	}
	if err := opts.Retention.validate(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("usage store: open %q: %w", path, err)
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: open %q: %w", path, err)
	}
	if err = migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: %q: %w", path, err)
	}
	s := newStore(db, time.Now)
	s.retention.Store(&opts.Retention)
	go s.run()
	return s, nil
}

func newStore(db *sql.DB, now func() time.Time) *Store {
	return &Store{
		db:   db,
		now:  now,
		in:   make(chan row, inputBuffer),
		done: make(chan struct{}),
	}
}

func dataSourceName(path string) string {
	query := url.Values{}
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)"} {
		query.Add("_pragma", pragma)
	}
	return "file:" + path + "?" + query.Encode()
}

// SetRetention replaces the retention used by the next hourly cleanup.
func (s *Store) SetRetention(retention Retention) error {
	if err := retention.validate(); err != nil {
		return err
	}
	s.retention.Store(&retention)
	return nil
}

// DroppedRecords counts records discarded because the writer fell behind or
// the store was closed.
func (s *Store) DroppedRecords() uint64 { return s.dropped.Load() }

// WriteErrors counts records lost because a write transaction failed.
func (s *Store) WriteErrors() uint64 { return s.writeErrors.Load() }

// HandleUsage implements usage.Plugin.
func (s *Store) HandleUsage(ctx context.Context, record coreusage.Record) {
	r := newRow(coreusage.Resolve(ctx, record))
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	select {
	case s.in <- r:
	default:
		s.recordDrop()
	}
}

func (s *Store) recordDrop() {
	total := s.dropped.Add(1)
	now := s.now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last >= int64(dropLogInterval) && s.lastDropLog.CompareAndSwap(last, now) {
		log.WithField("dropped_records", total).Warn("usage store: writer is behind; dropping usage records")
	}
}

// Close stops accepting records, waits until the writer has stored the
// pending ones and closes the database. When ctx ends first, the database
// stays open for the writer and Close returns ctx's error.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.in)
	}
	s.mu.Unlock()
	if err := waitDone(ctx, s.done); err != nil {
		return fmt.Errorf("usage store: wait for pending records: %w", err)
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("usage store: close database: %w", err)
	}
	return nil
}

// waitDone waits for done; a closed done wins over an already expired ctx.
func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) run() {
	defer close(s.done)
	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()
	retentionTicker := time.NewTicker(retentionInterval)
	defer retentionTicker.Stop()

	s.deleteExpiredAndLog()
	batch := make([]row, 0, maxBatchRows)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.write(batch); err != nil {
			s.writeErrors.Add(uint64(len(batch)))
			log.WithError(err).WithField("records", len(batch)).Error("usage store: write failed; records lost")
		}
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-s.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) == maxBatchRows {
				flush()
			}
		case <-flushTicker.C:
			flush()
		case <-retentionTicker.C:
			s.deleteExpiredAndLog()
		}
	}
}

func (s *Store) deleteExpiredAndLog() {
	if err := s.deleteExpired(context.Background()); err != nil {
		log.WithError(err).Error("usage store: retention cleanup failed")
	}
}

// row is one usage record in storage form.
type row struct {
	requestID       string
	at              time.Time
	provider        string
	authIndex       string
	model           string
	alias           string
	apiKeyFP        string
	sessionID       string
	parentSessionID string
	stream          bool
	failed          bool
	statusCode      int
	latency         time.Duration
	ttft            time.Duration
	tokens          Tokens
}

func newRow(resolved coreusage.Resolved) row {
	breakdown := resolved.Detail.TokenBreakdown
	return row{
		requestID:       resolved.RequestID,
		at:              resolved.RequestedAt,
		provider:        resolved.Provider,
		authIndex:       strings.TrimSpace(resolved.AuthIndex),
		model:           resolved.Model,
		alias:           strings.TrimSpace(resolved.Alias),
		apiKeyFP:        apiKeyFingerprint(resolved.APIKey),
		sessionID:       resolved.SessionID,
		parentSessionID: resolved.ParentSessionID,
		stream:          resolved.Stream,
		failed:          resolved.Failed,
		statusCode:      resolved.Fail.StatusCode,
		latency:         resolved.Latency,
		ttft:            resolved.TTFT,
		tokens: Tokens{
			Input:        breakdown.Input.UncachedTokens,
			CacheRead:    breakdown.Input.CacheReadTokens,
			CacheWrite:   breakdown.Input.CacheWriteTokens,
			Output:       breakdown.Output.NonReasoningTokens,
			Reasoning:    breakdown.Output.ReasoningTokens,
			Unclassified: breakdown.UnclassifiedTokens,
		},
	}
}

// apiKeyFingerprint identifies a client API key without storing it.
func apiKeyFingerprint(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])[:12]
}

const insertRequestSQL = `INSERT OR IGNORE INTO requests (
	request_id, ts_ms, provider, auth_index, model, alias, api_key_fp, session_id, parent_session_id,
	stream, failed, status_code, latency_ms, ttft_ms,
	input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, unclassified_tokens
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const upsertHourlySQL = `INSERT INTO usage_hourly (
	hour_ms, provider, auth_index, model, api_key_fp, requests, failed,
	input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, unclassified_tokens
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (hour_ms, provider, auth_index, model, api_key_fp) DO UPDATE SET
	requests = requests + 1,
	failed = failed + excluded.failed,
	input_tokens = input_tokens + excluded.input_tokens,
	cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
	cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
	output_tokens = output_tokens + excluded.output_tokens,
	reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
	unclassified_tokens = unclassified_tokens + excluded.unclassified_tokens`

// write stores a batch in one transaction. The hourly totals change only for
// rows that were inserted, so a repeated request ID is counted once.
func (s *Store) write(batch []row) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	insertRequest, err := tx.PrepareContext(ctx, insertRequestSQL)
	if err != nil {
		return fmt.Errorf("prepare request insert: %w", err)
	}
	upsertHourly, err := tx.PrepareContext(ctx, upsertHourlySQL)
	if err != nil {
		return fmt.Errorf("prepare hourly upsert: %w", err)
	}
	for _, r := range batch {
		t := r.tokens
		result, errInsert := insertRequest.ExecContext(ctx,
			r.requestID, r.at.UnixMilli(), r.provider, r.authIndex, r.model, r.alias, r.apiKeyFP, r.sessionID, r.parentSessionID,
			r.stream, r.failed, r.statusCode, nullableMillis(r.latency), nullableMillis(r.ttft),
			t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified)
		if errInsert != nil {
			return fmt.Errorf("insert request %q: %w", r.requestID, errInsert)
		}
		inserted, errAffected := result.RowsAffected()
		if errAffected != nil {
			return fmt.Errorf("insert request %q: %w", r.requestID, errAffected)
		}
		if inserted == 0 {
			continue
		}
		if _, errUpsert := upsertHourly.ExecContext(ctx,
			hourStart(r.at).UnixMilli(), r.provider, r.authIndex, r.model, r.apiKeyFP, r.failed,
			t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified); errUpsert != nil {
			return fmt.Errorf("update hourly totals for %q: %w", r.requestID, errUpsert)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func nullableMillis(d time.Duration) sql.NullInt64 {
	if d <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: d.Milliseconds(), Valid: true}
}

func hourStart(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}
