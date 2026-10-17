package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestPromptCacheTTL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		body     string
		want     time.Duration
	}{
		{"claude default", "claude", `{"messages":[{"role":"user","content":"hi"}]}`, 5 * time.Minute},
		{"claude 1h on system", "claude", `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`, time.Hour},
		{"claude 1h on a message", "claude", `{"messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, time.Hour},
		{"claude 5m explicit", "claude", `{"tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`, 5 * time.Minute},
		{"codex keeps caches too long to tell", "codex", `{"input":[]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptCacheTTL(tc.provider, []byte(tc.body)); got != tc.want {
				t.Fatalf("promptCacheTTL = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFreeMoveReason(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	prev := sessionContext{anchor: "a", seenAt: at, cacheTTL: 5 * time.Minute}
	for _, tc := range []struct {
		name string
		prev sessionContext
		next sessionContext
		want string
	}{
		{"same context within the cache", prev, sessionContext{anchor: "a", seenAt: at.Add(4 * time.Minute)}, ""},
		{"compaction", prev, sessionContext{anchor: "b", seenAt: at.Add(time.Minute)}, "context changed"},
		{"idle past the cache", prev, sessionContext{anchor: "a", seenAt: at.Add(6 * time.Minute)}, "prompt cache expired"},
		{"no anchor to compare", sessionContext{seenAt: at}, sessionContext{anchor: "b", seenAt: at.Add(time.Minute)}, ""},
		{"cache lifetime unknown", sessionContext{anchor: "a", seenAt: at}, sessionContext{anchor: "a", seenAt: at.Add(time.Hour)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := freeMoveReason(tc.prev, tc.next); got != tc.want {
				t.Fatalf("freeMoveReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// freeMoveFixture is a session-affinity selector over a pace selector with two Claude
// accounts whose seven-day windows the test sets.
type freeMoveFixture struct {
	selector *SessionAffinitySelector
	cache    *quota.Cache
	auths    []*Auth
}

func newFreeMoveFixture(t *testing.T) *freeMoveFixture {
	t.Helper()
	cache := quota.NewCache()
	pace := NewPaceSelector(PaceConfig{PaceTolerance: 0.1}, cache)
	pace.intn = func(int) int { return 0 }
	return &freeMoveFixture{
		selector: NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: pace, TTL: time.Hour}),
		cache:    cache,
		auths:    []*Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}},
	}
}

// setPace gives authID a seven-day window at remaining with half the week left.
func (f *freeMoveFixture) setPace(authID string, remaining float64) {
	now := time.Now()
	f.cache.PutProbe(authID, []quota.Window{{ID: quota.SevenDay, Remaining: remaining, ResetAt: now.Add(week / 2), Length: week, ObservedAt: now, Source: quota.SourceProbe}})
}

func (f *freeMoveFixture) pick(t *testing.T, firstTurn string) string {
	t.Helper()
	headers := http.Header{}
	headers.Set("X-Session-ID", "session-1")
	body := `{"system":"You are helpful.","messages":[{"role":"user","content":"` + firstTurn + `"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`
	auth, err := f.selector.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{
		Headers:         headers,
		OriginalRequest: []byte(body),
		SourceFormat:    sdktranslator.FormatClaude,
	}, f.auths)
	if err != nil || auth == nil {
		t.Fatalf("Pick: %v, %v", auth, err)
	}
	return auth.ID
}

// ageSessions moves every recorded request back by d, as if the session had been idle.
func (f *freeMoveFixture) ageSessions(d time.Duration) {
	f.selector.contexts.mu.Lock()
	defer f.selector.contexts.mu.Unlock()
	for key, entry := range f.selector.contexts.entries {
		entry.seenAt = entry.seenAt.Add(-d)
		f.selector.contexts.entries[key] = entry
	}
}

func TestSessionMovesToABetterPacedAccountOnlyAtAFreePoint(t *testing.T) {
	f := newFreeMoveFixture(t)
	f.setPace("a", 0.9)
	f.setPace("b", 0.2)
	if got := f.pick(t, "task one"); got != "a" {
		t.Fatalf("first pick = %s, want a", got)
	}

	// a falls behind; the session keeps it while its prompt cache is warm.
	f.setPace("a", 0.1)
	f.setPace("b", 0.9)
	if got := f.pick(t, "task one"); got != "a" {
		t.Fatalf("warm cache pick = %s, want the bound account a", got)
	}

	// Compaction replaced the first turn: the cache is gone, so the session moves.
	if got := f.pick(t, "summary of task one"); got != "b" {
		t.Fatalf("after compaction = %s, want b", got)
	}
	if got := f.pick(t, "summary of task one"); got != "b" {
		t.Fatalf("after the move = %s, want the new binding b", got)
	}

	// Idle past Claude's 5-minute cache while b falls behind: the session moves back.
	f.setPace("a", 0.9)
	f.setPace("b", 0.1)
	f.ageSessions(6 * time.Minute)
	if got := f.pick(t, "summary of task one"); got != "a" {
		t.Fatalf("after an idle gap = %s, want a", got)
	}
}

func TestSessionKeepsATopBandAccountAtAFreePoint(t *testing.T) {
	f := newFreeMoveFixture(t)
	f.setPace("a", 0.9)
	f.setPace("b", 0.2)
	if got := f.pick(t, "task one"); got != "a" {
		t.Fatalf("first pick = %s, want a", got)
	}
	// b is now slightly ahead of a, but a is within the 10 % top band.
	f.setPace("b", 0.95)
	if got := f.pick(t, "summary of task one"); got != "a" {
		t.Fatalf("free point with a in the top band = %s, want a", got)
	}
}

func TestFreeMovesNeedARankingFallback(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	if selector.freeMoves != nil || selector.contexts != nil {
		t.Fatal("round-robin has no ranking to move sessions by")
	}
}
