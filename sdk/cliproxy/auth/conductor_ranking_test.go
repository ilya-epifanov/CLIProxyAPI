package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// newPaceManager registers claude accounts serving models and returns a manager
// whose selector is selector, plus the quota cache selector reads.
func newPaceManager(t *testing.T, selector func(*quota.Cache) Selector, models []string, auths ...*Auth) (*Manager, *quota.Cache) {
	t.Helper()
	cache := quota.NewCache()
	manager := NewManager(nil, selector(cache), nil)
	manager.executors["claude"] = schedulerTestExecutor{provider: "claude"}
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		infos := make([]*registry.ModelInfo, 0, len(models))
		for _, model := range models {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		reg.RegisterClient(auth.ID, "claude", infos)
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s): %v", auth.ID, errRegister)
		}
	}
	return manager, cache
}

func paceSelectorFor(cache *quota.Cache) Selector {
	return NewPaceSelector(PaceConfig{BurstReserve: 0.2, PaceTolerance: 0.1, StaleAfter: 15 * time.Minute}, cache)
}

// liveWindow is a probe window relative to the wall clock, which Manager paths use.
func liveWindow(id quota.WindowID, remaining float64, resetIn time.Duration) quota.Window {
	now := time.Now()
	return quota.Window{ID: id, Remaining: remaining, ResetAt: now.Add(resetIn), Length: week, ObservedAt: now, Source: quota.SourceProbe}
}

func TestManagerPaceResolvesAliasToFable(t *testing.T) {
	const alias, fable = "fast", "claude-fable-5-1"
	manager, cache := newPaceManager(t, paceSelectorFor, []string{alias, fable},
		&Auth{ID: "a", Provider: "claude", Status: StatusActive},
		&Auth{ID: "b", Provider: "claude", Status: StatusActive},
	)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: fable, Alias: alias, Fork: true}},
	})
	// a leads on the shared 7-day window but its Fable window is empty.
	cache.PutProbe("a", []quota.Window{liveWindow(quota.SevenDay, 0.75, 84*time.Hour), liveWindow(quota.SevenDayFable, 0, 20*time.Hour)})
	cache.PutProbe("b", []quota.Window{liveWindow(quota.SevenDay, 0.5, 84*time.Hour), liveWindow(quota.SevenDayFable, 0.5, 84*time.Hour)})

	picked, _, errPick := manager.pickNext(context.Background(), "claude", alias, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext: %v", errPick)
	}
	if picked.ID != "b" {
		t.Fatalf("picked %s for alias %q, want b: the alias resolves to Fable and a's Fable window is empty", picked.ID, alias)
	}

	ranking := manager.RankCandidates(context.Background(), "claude", alias)
	if !ranking.Ranked || len(ranking.Entries) != 2 {
		t.Fatalf("ranking = %+v", ranking)
	}
	assertOrder(t, ranking.Entries, "b", "a")
	if a := ranking.Entries[1]; a.Class != ClassExhausted || a.ExhaustedWindow != quota.SevenDayFable {
		t.Fatalf("a = %+v, want exhausted on seven-day-fable", a)
	}

	credentialScope := manager.RankCandidates(context.Background(), "claude", "")
	assertOrder(t, credentialScope.Entries, "a", "b")
	if lw := credentialScope.Entries[0].LongWindows; len(lw) != 1 || lw[0] != quota.SevenDay {
		t.Fatalf("credential-scope long windows = %v, want [seven-day]", lw)
	}
}

func TestManagerRankCandidatesTiersAndNotCandidates(t *testing.T) {
	affinity := func(cache *quota.Cache) Selector { return NewSessionAffinitySelector(paceSelectorFor(cache)) }
	manager, cache := newPaceManager(t, affinity, []string{"claude-sonnet-4-5"},
		&Auth{ID: "pinned", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"priority": "1"}},
		&Auth{ID: "normal-a", Provider: "claude", Status: StatusActive},
		&Auth{ID: "normal-b", Provider: "claude", Status: StatusActive},
		&Auth{ID: "off", Provider: "claude", Status: StatusDisabled, Disabled: true},
	)
	if stopper, ok := manager.Selector().(*SessionAffinitySelector); ok {
		t.Cleanup(stopper.Stop)
	}
	cache.PutProbe("pinned", []quota.Window{liveWindow(quota.SevenDay, 0.1, 84*time.Hour)})
	cache.PutProbe("normal-a", []quota.Window{liveWindow(quota.SevenDay, 0.9, 84*time.Hour)})
	cache.PutProbe("normal-b", []quota.Window{liveWindow(quota.SevenDay, 0.88, 84*time.Hour)})

	ranking := manager.RankCandidates(context.Background(), "claude", "")
	if !ranking.Ranked {
		t.Fatal("ranking through the affinity wrapper must be ranked")
	}
	assertOrder(t, ranking.Entries, "pinned", "normal-a", "normal-b")
	if !ranking.Entries[0].TopBand || ranking.Entries[1].TopBand || ranking.Entries[2].TopBand {
		t.Fatal("top band must be limited to the highest tier")
	}
	if ranking.Entries[0].Priority != 1 {
		t.Fatalf("pinned priority = %d, want 1", ranking.Entries[0].Priority)
	}
	if len(ranking.NotCandidates) != 1 || ranking.NotCandidates[0].Auth.ID != "off" || ranking.NotCandidates[0].Reason != "disabled" {
		t.Fatalf("not candidates = %+v, want off/disabled", ranking.NotCandidates)
	}
}

func TestManagerRankCandidatesUnrankedStrategy(t *testing.T) {
	roundRobin := func(*quota.Cache) Selector { return &RoundRobinSelector{} }
	manager, _ := newPaceManager(t, roundRobin, []string{"claude-sonnet-4-5"},
		&Auth{ID: "b", Provider: "claude", Status: StatusActive},
		&Auth{ID: "a", Provider: "claude", Status: StatusActive},
	)
	ranking := manager.RankCandidates(context.Background(), "claude", "")
	if ranking.Ranked {
		t.Fatal("round-robin must not report a ranking")
	}
	assertOrder(t, ranking.Entries, "a", "b")
}
