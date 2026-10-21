package usagestore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
	// Embedded zone data, so day buckets work on hosts without a zoneinfo database.
	_ "time/tzdata"
)

const (
	maxHourlySpan = 31 * 24 * time.Hour
	maxSpan       = 731 * 24 * time.Hour
)

// Tokens holds the canonical, non-overlapping token buckets: Input excludes
// cached input and Output excludes reasoning, so their sum is the total.
type Tokens struct {
	Input        int64 `json:"input"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	Output       int64 `json:"output"`
	Reasoning    int64 `json:"reasoning"`
	Unclassified int64 `json:"unclassified"`
}

// Counts aggregates requests and tokens.
type Counts struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
	Tokens
}

func (c *Counts) add(other Counts) {
	c.Requests += other.Requests
	c.Failed += other.Failed
	c.Input += other.Input
	c.CacheRead += other.CacheRead
	c.CacheWrite += other.CacheWrite
	c.Output += other.Output
	c.Reasoning += other.Reasoning
	c.Unclassified += other.Unclassified
}

// Dimension is a column that usage can be grouped by.
type Dimension int

const (
	DimensionProvider Dimension = iota
	DimensionAuthIndex
	DimensionModel
)

// String returns the wire name, which is also the column name.
func (d Dimension) String() string {
	switch d {
	case DimensionAuthIndex:
		return "auth_index"
	case DimensionModel:
		return "model"
	default:
		return "provider"
	}
}

func parseDimension(name string) (Dimension, bool) {
	for _, d := range []Dimension{DimensionProvider, DimensionAuthIndex, DimensionModel} {
		if d.String() == name {
			return d, true
		}
	}
	return 0, false
}

// Bucket is the width of a series point.
type Bucket int

const (
	BucketHour Bucket = iota
	BucketDay
)

// String returns the wire name.
func (b Bucket) String() string {
	if b == BucketDay {
		return "1d"
	}
	return "1h"
}

// Range is a half-open interval [From, To) of whole UTC hours.
type Range struct {
	From time.Time
	To   time.Time
}

// SeriesQuery selects usage over time grouped by one dimension.
type SeriesQuery struct {
	Range    Range
	Bucket   Bucket
	Location *time.Location
	GroupBy  Dimension
}

// QueryError reports an invalid query parameter.
type QueryError struct {
	Param  string
	Reason string
}

func (e *QueryError) Error() string { return e.Param + ": " + e.Reason }

// ParseRange parses RFC 3339 bounds. From is aligned down and To up to whole
// hours, so the current partial hour is included.
func ParseRange(from, to string) (Range, error) {
	fromTime, err := time.Parse(time.RFC3339, strings.TrimSpace(from))
	if err != nil {
		return Range{}, &QueryError{Param: "from", Reason: "must be an RFC 3339 time"}
	}
	toTime, err := time.Parse(time.RFC3339, strings.TrimSpace(to))
	if err != nil {
		return Range{}, &QueryError{Param: "to", Reason: "must be an RFC 3339 time"}
	}
	// Checked before alignment, which would turn an inverted range inside one hour into that hour.
	if !fromTime.Before(toTime) {
		return Range{}, &QueryError{Param: "to", Reason: "must be after from"}
	}
	r := Range{From: hourStart(fromTime), To: hourStart(toTime)}
	if !toTime.Equal(r.To) {
		r.To = r.To.Add(time.Hour)
	}
	if r.To.Sub(r.From) > maxSpan {
		return Range{}, &QueryError{Param: "from", Reason: "range must not exceed 731 days"}
	}
	return r, nil
}

// ParseSeriesQuery parses the parameters of a series query.
func ParseSeriesQuery(from, to, bucket, tz, groupBy string) (SeriesQuery, error) {
	r, err := ParseRange(from, to)
	if err != nil {
		return SeriesQuery{}, err
	}
	q := SeriesQuery{Range: r}
	switch strings.TrimSpace(bucket) {
	case "1h":
		q.Bucket = BucketHour
		if r.To.Sub(r.From) > maxHourlySpan {
			return SeriesQuery{}, &QueryError{Param: "bucket", Reason: "1h buckets need a range of at most 31 days"}
		}
	case "1d":
		q.Bucket = BucketDay
	default:
		return SeriesQuery{}, &QueryError{Param: "bucket", Reason: "must be 1h or 1d"}
	}
	q.Location = time.UTC
	if name := strings.TrimSpace(tz); name != "" {
		if q.Location, err = time.LoadLocation(name); err != nil {
			return SeriesQuery{}, &QueryError{Param: "tz", Reason: "must be an IANA time zone name"}
		}
	}
	var ok bool
	if q.GroupBy, ok = parseDimension(strings.TrimSpace(groupBy)); !ok {
		return SeriesQuery{}, &QueryError{Param: "group_by", Reason: "must be provider, auth_index or model"}
	}
	return q, nil
}

// Point is one bucket of a series. T is the bucket start.
type Point struct {
	T time.Time `json:"t"`
	Counts
}

// Series holds the non-empty buckets of one group, ordered by time.
type Series struct {
	Key    string  `json:"key"`
	Points []Point `json:"points"`
}

// TotalsRow is the usage of one provider, credential and model over a range.
type TotalsRow struct {
	Provider  string `json:"provider"`
	AuthIndex string `json:"auth_index"`
	Model     string `json:"model"`
	Counts
}

const sumColumns = `SUM(requests), SUM(failed), SUM(input_tokens), SUM(cache_read_tokens), SUM(cache_write_tokens),
	SUM(output_tokens), SUM(reasoning_tokens), SUM(unclassified_tokens)`

func countsDest(c *Counts) []any {
	return []any{&c.Requests, &c.Failed, &c.Input, &c.CacheRead, &c.CacheWrite, &c.Output, &c.Reasoning, &c.Unclassified}
}

// Series returns usage over time, one series per value of q.GroupBy, ordered by key.
func (s *Store) Series(ctx context.Context, q SeriesQuery) ([]Series, error) {
	column := q.GroupBy.String()
	rows, err := s.db.QueryContext(ctx,
		`SELECT hour_ms, `+column+`, `+sumColumns+` FROM usage_hourly
		WHERE hour_ms >= ? AND hour_ms < ? GROUP BY hour_ms, `+column,
		q.Range.From.UnixMilli(), q.Range.To.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("usage store: query series: %w", err)
	}
	defer func() { _ = rows.Close() }()

	buckets := map[string]map[int64]*Counts{}
	for rows.Next() {
		var hourMs int64
		var key string
		var counts Counts
		if err = rows.Scan(append([]any{&hourMs, &key}, countsDest(&counts)...)...); err != nil {
			return nil, fmt.Errorf("usage store: read series: %w", err)
		}
		start := bucketStart(time.UnixMilli(hourMs), q.Bucket, q.Location).UnixMilli()
		points := buckets[key]
		if points == nil {
			points = map[int64]*Counts{}
			buckets[key] = points
		}
		if points[start] == nil {
			points[start] = &Counts{}
		}
		points[start].add(counts)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("usage store: read series: %w", err)
	}

	series := make([]Series, 0, len(buckets))
	for key, points := range buckets {
		out := Series{Key: key, Points: make([]Point, 0, len(points))}
		for startMs, counts := range points {
			out.Points = append(out.Points, Point{T: time.UnixMilli(startMs).UTC(), Counts: *counts})
		}
		sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].T.Before(out.Points[j].T) })
		series = append(series, out)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Key < series[j].Key })
	return series, nil
}

// bucketStart maps the start of an hour to the start of its bucket. A day
// starts at local midnight in loc; an hour that starts before a local
// midnight belongs to the day it starts in.
func bucketStart(hour time.Time, bucket Bucket, loc *time.Location) time.Time {
	if bucket == BucketHour {
		return hour
	}
	local := hour.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// Totals returns usage per provider, credential and model over r.
func (s *Store) Totals(ctx context.Context, r Range) ([]TotalsRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, auth_index, model, `+sumColumns+` FROM usage_hourly
		WHERE hour_ms >= ? AND hour_ms < ? GROUP BY provider, auth_index, model
		ORDER BY provider, auth_index, model`,
		r.From.UnixMilli(), r.To.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("usage store: query totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	totals := []TotalsRow{}
	for rows.Next() {
		var total TotalsRow
		if err = rows.Scan(append([]any{&total.Provider, &total.AuthIndex, &total.Model}, countsDest(&total.Counts)...)...); err != nil {
			return nil, fmt.Errorf("usage store: read totals: %w", err)
		}
		totals = append(totals, total)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("usage store: read totals: %w", err)
	}
	return totals, nil
}

// RequestUsage is one stored request: when, on which account, and its tokens.
type RequestUsage struct {
	At        time.Time
	Provider  string
	AuthIndex string
	Model     string
	SessionID string
	Tokens    Tokens
}

// RequestsSince calls fn for each stored request from from on, oldest first, and returns
// the instant from which the store holds every request: from, unless the request
// retention or the oldest stored request is later.
func (s *Store) RequestsSince(ctx context.Context, from time.Time, fn func(RequestUsage)) (time.Time, error) {
	covered := from
	if cutoff := s.now().AddDate(0, 0, -s.retention.Load().RequestDays); cutoff.After(covered) {
		covered = cutoff
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(ts_ms) FROM requests`).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("usage store: query oldest request: %w", err)
	}
	if !oldest.Valid {
		return s.now(), nil
	}
	if first := time.UnixMilli(oldest.Int64); first.After(covered) {
		covered = first
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts_ms, provider, auth_index, model, session_id, input_tokens, cache_read_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens, unclassified_tokens
		FROM requests WHERE ts_ms >= ? ORDER BY ts_ms`, from.UnixMilli())
	if err != nil {
		return time.Time{}, fmt.Errorf("usage store: query requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var atMs int64
		var r RequestUsage
		t := &r.Tokens
		if err = rows.Scan(&atMs, &r.Provider, &r.AuthIndex, &r.Model, &r.SessionID,
			&t.Input, &t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified); err != nil {
			return time.Time{}, fmt.Errorf("usage store: read requests: %w", err)
		}
		r.At = time.UnixMilli(atMs)
		fn(r)
	}
	if err = rows.Err(); err != nil {
		return time.Time{}, fmt.Errorf("usage store: read requests: %w", err)
	}
	return covered, nil
}
