// Package usagemetrics counts upstream requests and tokens per provider,
// credential and model, and exposes the counts to Prometheus.
package usagemetrics

import (
	"context"
	"strings"
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type seriesKey struct {
	provider  string
	authIndex string
	model     string
}

type counters struct {
	requestsByCode map[int]uint64
	tokens         coreusage.TokenBuckets
}

// Aggregator is a usage.Plugin that keeps counters since process start.
type Aggregator struct {
	mu     sync.Mutex
	series map[seriesKey]*counters
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{series: map[seriesKey]*counters{}}
}

// HandleUsage implements usage.Plugin.
func (a *Aggregator) HandleUsage(ctx context.Context, record coreusage.Record) {
	resolved := coreusage.Resolve(ctx, record)
	key := seriesKey{
		provider:  resolved.Provider,
		authIndex: strings.TrimSpace(resolved.AuthIndex),
		model:     resolved.Model,
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.series[key]
	if c == nil {
		c = &counters{requestsByCode: map[int]uint64{}}
		a.series[key] = c
	}
	c.requestsByCode[resolved.Fail.StatusCode]++
	// Resolve returns a valid breakdown, which has no negative buckets (TokenBreakdown.Valid).
	c.tokens = c.tokens.Add(resolved.Detail.TokenBreakdown.Buckets())
}

type seriesSnapshot struct {
	key            seriesKey
	requestsByCode map[int]uint64
	tokens         coreusage.TokenBuckets
}

// snapshot copies the counters.
func (a *Aggregator) snapshot() []seriesSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]seriesSnapshot, 0, len(a.series))
	for key, c := range a.series {
		codes := make(map[int]uint64, len(c.requestsByCode))
		for code, n := range c.requestsByCode {
			codes[code] = n
		}
		out = append(out, seriesSnapshot{key: key, requestsByCode: codes, tokens: c.tokens})
	}
	return out
}
