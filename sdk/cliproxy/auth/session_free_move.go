package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
)

// Free moves: a bound session normally keeps its account so that its prompt cache stays
// warm. When that cache is gone anyway, moving the session costs nothing, so the session
// goes back to the ranking selector. The cache is gone when
//   - the conversation's first non-system turn changed (compaction, a cleared context), or
//   - the session was idle longer than the prompt cache lives (Claude: 5 minutes, or 1 hour
//     when the request asks for it).
//
// OpenAI keeps prompt caches for 30 minutes up to 24 hours, so for Codex only the first
// signal counts.

const (
	claudePromptCacheTTL     = 5 * time.Minute
	claudePromptCacheLongTTL = time.Hour
	// sessionContextPruneEvery is how many records pass between sweeps of expired ones.
	sessionContextPruneEvery = 1024
)

// sessionContext is what the tracker remembers about a session's latest request.
type sessionContext struct {
	// anchor fingerprints the first non-system turn; "" when the request has none.
	anchor string
	seenAt time.Time
	// cacheTTL is how long the provider keeps the prompt cache after this request;
	// 0 when that is unknown or too long to tell.
	cacheTTL time.Duration
}

// sessionContextTracker remembers, per session binding, the latest request's context.
type sessionContextTracker struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]sessionContext
	records int
}

func newSessionContextTracker(ttl time.Duration) *sessionContextTracker {
	return &sessionContextTracker{ttl: ttl, entries: map[string]sessionContext{}}
}

// swap records next for key and returns the previous record, if any. Records older than
// the binding TTL are swept from time to time.
func (t *sessionContextTracker) swap(key string, next sessionContext) (sessionContext, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.entries[key]
	t.entries[key] = next
	t.records++
	if t.records%sessionContextPruneEvery == 0 {
		for k, entry := range t.entries {
			if next.seenAt.Sub(entry.seenAt) > t.ttl {
				delete(t.entries, k)
			}
		}
	}
	return prev, ok
}

// freeMoveReason says why the bound account no longer holds the session's prompt cache,
// or returns "" when it may still hold it.
func freeMoveReason(prev, next sessionContext) string {
	if prev.anchor != "" && next.anchor != "" && prev.anchor != next.anchor {
		return "context changed"
	}
	if prev.cacheTTL > 0 && next.seenAt.Sub(prev.seenAt) > prev.cacheTTL {
		return "prompt cache expired"
	}
	return ""
}

// sessionContextOf reads the request's anchor and prompt-cache lifetime.
func sessionContextOf(provider string, opts cliproxyexecutor.Options, now time.Time) sessionContext {
	out := sessionContext{seenAt: now, cacheTTL: promptCacheTTL(provider, opts.OriginalRequest)}
	for _, turn := range cliproxysession.ExtractCanonicalTurns(opts.SourceFormat, opts.OriginalRequest) {
		if role := strings.ToLower(strings.TrimSpace(turn.Role)); role != "system" && role != "developer" {
			out.anchor = cliproxysession.FastTurnFingerprint(turn)
			break
		}
	}
	return out
}

// promptCacheTTL returns how long provider keeps the prompt cache written by payload.
func promptCacheTTL(provider string, payload []byte) time.Duration {
	if !strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return 0
	}
	for _, path := range []string{"system.#.cache_control.ttl", "tools.#.cache_control.ttl", "messages.#.content.#.cache_control.ttl|@flatten"} {
		for _, ttl := range gjson.GetBytes(payload, path).Array() {
			if ttl.String() == "1h" {
				return claudePromptCacheLongTTL
			}
		}
	}
	return claudePromptCacheTTL
}

// placeAtFreePoint re-places a session whose prompt cache on bound is gone. The session
// keeps bound when it is in the ranking selector's top band: Claude Code's shared system
// prompt and tools are cached per account, so even a free move is not quite free.
func (s *SessionAffinitySelector) placeAtFreePoint(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, bound *Auth, candidates []*Auth, now time.Time) (*Auth, error) {
	for _, ranked := range s.freeMoves.Rank(ctx, candidates, model, now) {
		if ranked.Auth.ID == bound.ID && ranked.TopBand {
			return bound, nil
		}
	}
	return s.fallback.Pick(ctx, provider, model, opts, candidates)
}
