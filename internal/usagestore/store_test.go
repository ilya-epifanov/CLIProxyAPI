package usagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var baseTime = time.Date(2026, 10, 9, 10, 15, 0, 0, time.UTC)

// newTestStore returns a migrated store without a writer goroutine, so tests
// call write directly and control time.
func newTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", dataSourceName(filepath.Join(t.TempDir(), "usage.db")))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := newStore(db, func() time.Time { return now })
	if err = s.SetRetention(Retention{RequestDays: 30, RollupDays: 730}); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	return s
}

func testRecord(id string, at time.Time, provider, authIndex, model string) coreusage.Record {
	return coreusage.Record{
		RequestID:   id,
		RequestedAt: at,
		Provider:    provider,
		AuthIndex:   authIndex,
		Model:       model,
		APIKey:      "client-key",
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:     1000,
			CacheReadTokens: 600,
			OutputTokens:    300,
			ReasoningTokens: 100,
		},
	}
}

func rowsOf(records ...coreusage.Record) []row {
	out := make([]row, 0, len(records))
	for _, record := range records {
		out = append(out, newRow(coreusage.Resolve(context.Background(), record)))
	}
	return out
}

func mustWrite(t *testing.T, s *Store, records ...coreusage.Record) {
	t.Helper()
	if err := s.write(rowsOf(records...)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func dayRange(t *testing.T) Range {
	t.Helper()
	r, err := ParseRange("2026-10-09T00:00:00Z", "2026-10-10T00:00:00Z")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	return r
}

func TestWriteStoresCanonicalTokenBuckets(t *testing.T) {
	s := newTestStore(t, baseTime)
	mustWrite(t, s, testRecord("r1", baseTime, "codex", "a1", "gpt-5"))

	totals, err := s.Totals(context.Background(), dayRange(t))
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	// Codex reports cached input inside input and reasoning inside output.
	want := []TotalsRow{{Provider: "codex", AuthIndex: "a1", Model: "gpt-5", Counts: Counts{
		Requests: 1,
		Tokens:   Tokens{Input: 400, CacheRead: 600, Output: 200, Reasoning: 100},
	}}}
	if !reflect.DeepEqual(totals, want) {
		t.Fatalf("Totals = %+v, want %+v", totals, want)
	}

	var fp string
	var latency sql.NullInt64
	var ttft sql.NullInt64
	if err = s.db.QueryRow(`SELECT api_key_fp, latency_ms, ttft_ms FROM requests`).Scan(&fp, &latency, &ttft); err != nil {
		t.Fatalf("read request row: %v", err)
	}
	if len(fp) != 12 || fp == "client-key" {
		t.Fatalf("api_key_fp = %q, want a 12-character fingerprint", fp)
	}
	if latency != (sql.NullInt64{Int64: 1500, Valid: true}) || ttft.Valid {
		t.Fatalf("latency = %+v, ttft = %+v", latency, ttft)
	}
}

func TestWriteCountsARepeatedRequestIDOnce(t *testing.T) {
	s := newTestStore(t, baseTime)
	record := testRecord("same", baseTime, "claude", "a1", "claude-fable-5")
	mustWrite(t, s, record, record)
	mustWrite(t, s, record)

	totals, err := s.Totals(context.Background(), dayRange(t))
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if len(totals) != 1 || totals[0].Requests != 1 {
		t.Fatalf("Totals = %+v, want one request", totals)
	}
	if n := countRows(t, s, "requests"); n != 1 {
		t.Fatalf("request rows = %d, want 1", n)
	}
}

func TestWriteAccumulatesHourlyTotalsAndFailures(t *testing.T) {
	s := newTestStore(t, baseTime)
	failed := testRecord("r3", baseTime.Add(20*time.Minute), "claude", "a1", "claude-fable-5")
	failed.Failed = true
	failed.Fail = coreusage.Failure{StatusCode: http.StatusTooManyRequests}
	failed.Detail = coreusage.Detail{}
	mustWrite(t, s,
		testRecord("r1", baseTime, "claude", "a1", "claude-fable-5"),
		testRecord("r2", baseTime.Add(10*time.Minute), "claude", "a1", "claude-fable-5"),
		failed,
	)

	if n := countRows(t, s, "usage_hourly"); n != 1 {
		t.Fatalf("hourly rows = %d, want 1", n)
	}
	totals, err := s.Totals(context.Background(), dayRange(t))
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if got := totals[0].Counts; got.Requests != 3 || got.Failed != 1 {
		t.Fatalf("Counts = %+v, want 3 requests and 1 failure", got)
	}
	var status int
	if err = s.db.QueryRow(`SELECT status_code FROM requests WHERE request_id = 'r3'`).Scan(&status); err != nil || status != http.StatusTooManyRequests {
		t.Fatalf("status_code = %d (%v), want 429", status, err)
	}
}

func TestSeriesGroupsByEachDimension(t *testing.T) {
	s := newTestStore(t, baseTime)
	mustWrite(t, s,
		testRecord("r1", baseTime, "claude", "a1", "claude-fable-5"),
		testRecord("r2", baseTime, "claude", "a2", "claude-fable-5"),
		testRecord("r3", baseTime.Add(time.Hour), "codex", "a3", "gpt-5"),
	)
	tests := []struct {
		groupBy string
		want    map[string][]int64 // key → requests per point
	}{
		{"provider", map[string][]int64{"claude": {2}, "codex": {1}}},
		{"auth_index", map[string][]int64{"a1": {1}, "a2": {1}, "a3": {1}}},
		{"model", map[string][]int64{"claude-fable-5": {2}, "gpt-5": {1}}},
	}
	for _, tt := range tests {
		t.Run(tt.groupBy, func(t *testing.T) {
			q, err := ParseSeriesQuery("2026-10-09T00:00:00Z", "2026-10-10T00:00:00Z", "1h", "", tt.groupBy)
			if err != nil {
				t.Fatalf("ParseSeriesQuery: %v", err)
			}
			series, err := s.Series(context.Background(), q)
			if err != nil {
				t.Fatalf("Series: %v", err)
			}
			got := map[string][]int64{}
			for _, one := range series {
				for _, point := range one.Points {
					got[one.Key] = append(got[one.Key], point.Requests)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Series requests = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeriesDayBucketsFollowLocalMidnightAcrossDST(t *testing.T) {
	s := newTestStore(t, baseTime)
	// Europe/Amsterdam leaves summer time on 2026-10-25: that day is 25 hours long.
	mustWrite(t, s,
		testRecord("before", time.Date(2026, 10, 24, 21, 30, 0, 0, time.UTC), "claude", "a1", "m"), // 23:30 on Oct 24
		testRecord("start", time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), "claude", "a1", "m"),   // 00:00 on Oct 25
		testRecord("end", time.Date(2026, 10, 25, 22, 59, 0, 0, time.UTC), "claude", "a1", "m"),    // 23:59 on Oct 25
		testRecord("next", time.Date(2026, 10, 25, 23, 0, 0, 0, time.UTC), "claude", "a1", "m"),    // 00:00 on Oct 26
	)
	q, err := ParseSeriesQuery("2026-10-24T00:00:00Z", "2026-10-27T00:00:00Z", "1d", "Europe/Amsterdam", "provider")
	if err != nil {
		t.Fatalf("ParseSeriesQuery: %v", err)
	}
	series, err := s.Series(context.Background(), q)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	got := map[string]int64{}
	for _, point := range series[0].Points {
		got[point.T.Format(time.RFC3339)] = point.Requests
	}
	want := map[string]int64{
		"2026-10-23T22:00:00Z": 1,
		"2026-10-24T22:00:00Z": 2,
		"2026-10-25T23:00:00Z": 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("day buckets = %v, want %v", got, want)
	}
}

func TestParseRangeAlignsToWholeHours(t *testing.T) {
	r, err := ParseRange("2026-10-09T10:37:00+02:00", "2026-10-09T12:37:00+02:00")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if want := (Range{From: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC)}); r != want {
		t.Fatalf("ParseRange = %+v, want %+v", r, want)
	}
	r, err = ParseRange("2026-10-09T08:00:00Z", "2026-10-09T11:00:00Z")
	if err != nil || !r.To.Equal(time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("ParseRange with an aligned end = %+v, %v", r, err)
	}
}

func TestParseSeriesQueryRejectsInvalidParameters(t *testing.T) {
	const from, to = "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z"
	tests := []struct {
		name                                string
		from, to, bucket, tz, groupBy, want string
	}{
		{"bad from", "yesterday", to, "1h", "", "provider", "from"},
		{"bad to", from, "", "1h", "", "provider", "to"},
		{"empty range", to, from, "1h", "", "provider", "to"},
		{"inverted within one hour", "2026-10-01T10:45:00Z", "2026-10-01T10:15:00Z", "1h", "", "provider", "to"},
		{"zero length", "2026-10-01T10:30:00Z", "2026-10-01T10:30:00Z", "1h", "", "provider", "to"},
		{"range over limit", "2024-01-01T00:00:00Z", "2026-10-01T00:00:00Z", "1d", "", "provider", "from"},
		{"hourly range over limit", "2026-08-01T00:00:00Z", to, "1h", "", "provider", "bucket"},
		{"bad bucket", from, to, "1w", "", "provider", "bucket"},
		{"bad tz", from, to, "1d", "Mars/Olympus", "provider", "tz"},
		{"bad group", from, to, "1h", "", "api_key", "group_by"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSeriesQuery(tt.from, tt.to, tt.bucket, tt.tz, tt.groupBy)
			var queryErr *QueryError
			if !errors.As(err, &queryErr) || queryErr.Param != tt.want {
				t.Fatalf("ParseSeriesQuery error = %v, want a QueryError for %q", err, tt.want)
			}
		})
	}
}

func TestDeleteExpiredKeepsRollupsLongerThanRequests(t *testing.T) {
	now := baseTime
	s := newTestStore(t, now)
	if err := s.SetRetention(Retention{RequestDays: 30, RollupDays: 60}); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	mustWrite(t, s,
		testRecord("recent", now.AddDate(0, 0, -29), "claude", "a1", "m"),
		testRecord("old", now.AddDate(0, 0, -31), "claude", "a1", "m"),
		testRecord("ancient", now.AddDate(0, 0, -61), "claude", "a1", "m"),
	)

	if err := s.deleteExpired(context.Background()); err != nil {
		t.Fatalf("deleteExpired: %v", err)
	}
	if n := countRows(t, s, "requests"); n != 1 {
		t.Fatalf("request rows = %d, want 1", n)
	}
	if n := countRows(t, s, "usage_hourly"); n != 2 {
		t.Fatalf("hourly rows = %d, want 2", n)
	}
}

func TestHandleUsageCountsDropsWhenTheWriterIsBehind(t *testing.T) {
	s := newTestStore(t, baseTime) // no writer goroutine drains the channel
	for i := 0; i < inputBuffer+3; i++ {
		s.HandleUsage(context.Background(), testRecord(fmt.Sprint(i), baseTime, "claude", "a1", "m"))
	}
	if got := s.DroppedRecords(); got != 3 {
		t.Fatalf("DroppedRecords = %d, want 3", got)
	}
}

func TestCloseStoresPendingRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(context.Background(), Options{Path: path, Retention: Retention{RequestDays: 30, RollupDays: 730}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		s.HandleUsage(context.Background(), testRecord(fmt.Sprint(i), time.Now(), "claude", "a1", "m"))
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.HandleUsage(context.Background(), testRecord("late", time.Now(), "claude", "a1", "m"))
	if s.DroppedRecords() != 1 {
		t.Fatalf("DroppedRecords after Close = %d, want 1", s.DroppedRecords())
	}

	reopened, err := Open(context.Background(), Options{Path: path, Retention: Retention{RequestDays: 30, RollupDays: 730}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	if got := countRows(t, reopened, "requests"); got != n {
		t.Fatalf("stored requests = %d, want %d", got, n)
	}
}

func TestOpenRejectsRetentionBelowOneDay(t *testing.T) {
	_, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "usage.db"), Retention: Retention{RequestDays: 0, RollupDays: 730}})
	if err == nil {
		t.Fatal("Open accepted a zero request retention")
	}
}

func TestOpenFailsWhenTheDirectoryIsMissing(t *testing.T) {
	_, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "missing", "usage.db"), Retention: Retention{RequestDays: 30, RollupDays: 730}})
	if err == nil {
		t.Fatal("Open succeeded for a path in a missing directory")
	}
}

func TestOpenRejectsANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	_ = db.Close()
	if _, err = Open(context.Background(), Options{Path: path, Retention: Retention{RequestDays: 30, RollupDays: 730}}); err == nil {
		t.Fatal("Open accepted a newer schema version")
	}
}

// TestShutdownOrderKeepsQueuedRecords follows Service.Shutdown: the usage
// manager delivers its queue, then the store writes what it still holds.
func TestShutdownOrderKeepsQueuedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	retention := Retention{RequestDays: 30, RollupDays: 730}
	s, err := Open(context.Background(), Options{Path: path, Retention: retention})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	manager := coreusage.NewManager(0)
	manager.Register(s)
	const n = 1000
	for i := 0; i < n; i++ {
		manager.Publish(context.Background(), testRecord(fmt.Sprint(i), time.Now(), "claude", "a1", "m"))
	}
	if err = manager.StopAndWait(context.Background()); err != nil {
		t.Fatalf("StopAndWait: %v", err)
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(context.Background(), Options{Path: path, Retention: retention})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	if got := countRows(t, reopened, "requests"); got != n || s.DroppedRecords() != 0 {
		t.Fatalf("stored %d of %d records, dropped %d", got, n, s.DroppedRecords())
	}
}
