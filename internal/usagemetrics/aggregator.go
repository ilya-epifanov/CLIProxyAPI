// Package usagemetrics counts upstream requests and tokens per provider,
// credential and model, and exposes the counts to Prometheus.
package usagemetrics

import (
	"context"
	"strings"
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// tokenType is one canonical, non-overlapping token bucket.
type tokenType int

const (
	tokenInput tokenType = iota
	tokenCacheRead
	tokenCacheWrite
	tokenOutput
	tokenReasoning
	tokenUnclassified
	tokenTypeCount
)

var tokenTypeLabels = [tokenTypeCount]string{"input", "cache_read", "cache_write", "output", "reasoning", "unclassified"}

type seriesKey struct {
	provider  string
	authIndex string
	model     string
}

type counters struct {
	requestsByCode map[int]uint64
	tokens         [tokenTypeCount]uint64
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
	breakdown := resolved.Detail.TokenBreakdown
	tokens := [tokenTypeCount]int64{
		tokenInput:        breakdown.Input.UncachedTokens,
		tokenCacheRead:    breakdown.Input.CacheReadTokens,
		tokenCacheWrite:   breakdown.Input.CacheWriteTokens,
		tokenOutput:       breakdown.Output.NonReasoningTokens,
		tokenReasoning:    breakdown.Output.ReasoningTokens,
		tokenUnclassified: breakdown.UnclassifiedTokens,
	}
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
	for i, n := range tokens {
		// A valid breakdown has no negative buckets (TokenBreakdown.Valid).
		if n > 0 {
			c.tokens[i] += uint64(n)
		}
	}
}

type seriesSnapshot struct {
	key            seriesKey
	requestsByCode map[int]uint64
	tokens         [tokenTypeCount]uint64
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
