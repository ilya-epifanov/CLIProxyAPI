package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// drainSet holds the given account IDs.
func drainSet(ids ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func TestDrainPicksTheFlaggedAccountClosestToItsLimit(t *testing.T) {
	f := newPaceFixture(t)
	f.put("ahead", paceWindow(quota.SevenDay, 1, 12*time.Hour, week), paceWindow(quota.FiveHour, 1, time.Hour, fiveHour))
	// b holds less of its short window than c, but c holds less of its longest one.
	f.put("b", paceWindow(quota.SevenDay, 0.5, 84*time.Hour, week), paceWindow(quota.FiveHour, 0.25, time.Hour, fiveHour))
	f.put("c", paceWindow(quota.SevenDay, 0.3, 84*time.Hour, week), paceWindow(quota.FiveHour, 0.9, time.Hour, fiveHour))
	// "no-data" has no quota read.
	auths := []*Auth{paceAuth("ahead", "claude", 0), paceAuth("b", "claude", 0), paceAuth("c", "claude", 0), paceAuth("no-data", "claude", 0)}
	ctx := withDraining(context.Background(), drainSet("b", "c", "no-data"))

	ranked := f.selector.Rank(ctx, auths, "", paceNow)
	assertOrder(t, ranked, "c", "b", "no-data", "ahead")
	for i, r := range ranked {
		if r.TopBand != (i == 0) {
			t.Fatalf("top band of %s = %v; want only c", r.Auth.ID, r.TopBand)
		}
	}

	picked, err := f.selector.Pick(ctx, "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "c" || f.intnArg != 1 {
		t.Fatalf("Pick chose %s from %d; want c alone", picked.ID, f.intnArg)
	}
}

func TestLongestWindowsAreFoundByLength(t *testing.T) {
	month := 30 * 24 * time.Hour
	windows := quota.AccountWindows{
		Short:    paceWindow(quota.FiveHour, 0, time.Hour, fiveHour),
		HasShort: true,
		Long:     []quota.Window{paceWindow(quota.Weekly, 0.2, 84*time.Hour, week), paceWindow(quota.Monthly, 0.7, 300*time.Hour, month)},
		Complete: true,
	}
	if got := longestLeft(windows, paceNow); got != 0.7 {
		t.Fatalf("longest left = %v; want the monthly 0.7", got)
	}
	if longestSpent(windows, paceNow) {
		t.Fatal("a spent short window counted as the longest")
	}
	windows.Long[1].Remaining = 0
	if !longestSpent(windows, paceNow) {
		t.Fatal("a spent monthly window not counted")
	}
	windows.Complete = false
	if longestSpent(windows, paceNow) {
		t.Fatal("spent reported while a long slot is unobserved")
	}
}

// newDrainManager registers claude accounts with a pace selector and drain flags stored
// in a temporary file.
func newDrainManager(t *testing.T, ids ...string) (*Manager, *quota.Cache, string) {
	t.Helper()
	auths := make([]*Auth, 0, len(ids))
	for _, id := range ids {
		auths = append(auths, &Auth{ID: id, Provider: "claude", Status: StatusActive})
	}
	return newDrainManagerOf(t, paceSelectorFor, auths...)
}

func newDrainManagerOf(t *testing.T, selector func(*quota.Cache) Selector, auths ...*Auth) (*Manager, *quota.Cache, string) {
	t.Helper()
	manager, cache := newPaceManager(t, selector, []string{"claude-sonnet-4-5"}, auths...)
	if affinity, ok := manager.Selector().(*SessionAffinitySelector); ok {
		t.Cleanup(affinity.Stop)
	}
	path := filepath.Join(t.TempDir(), "drain-flags.json")
	flags, err := LoadDrainFlags(path)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetDrainFlags(flags)
	return manager, cache, path
}

func assertStoredFlags(t *testing.T, path string, want map[string]bool) {
	t.Helper()
	loaded, err := LoadDrainFlags(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for id, flagged := range want {
		if loaded.Has(id) != flagged {
			t.Fatalf("stored flag of %s = %v; want %v", id, !flagged, flagged)
		}
	}
}

func TestDrainClearsWhenAQuotaReadShowsTheLongestWindowUsedUp(t *testing.T) {
	manager, cache, path := newDrainManager(t, "a", "b")
	flags := manager.DrainFlags()
	for _, id := range []string{"a", "b"} {
		if err := flags.Set(id, true, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	// A spent short window leaves the flag: the account comes back at its reset.
	cache.PutProbe("a", []quota.Window{liveWindow(quota.SevenDay, 0.2, 84*time.Hour), {ID: quota.FiveHour, Remaining: 0, ResetAt: time.Now().Add(time.Hour), Length: fiveHour, ObservedAt: time.Now()}})
	manager.ClearSpentDrain("a")
	if !flags.Has("a") {
		t.Fatal("flag cleared on a spent short window")
	}

	cache.PutProbe("a", []quota.Window{liveWindow(quota.SevenDay, 0, 84*time.Hour)})
	manager.ClearSpentDrain("a")
	if flags.Has("a") {
		t.Fatal("flag kept after the longest window was used up")
	}
	assertStoredFlags(t, path, map[string]bool{"a": false, "b": true})
}

func TestDrainClearsWhenResponseHeadersShowTheLongestWindowUsedUp(t *testing.T) {
	manager, _, path := newDrainManager(t, "a")
	if err := manager.DrainFlags().Set("a", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.4"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"1"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       []string{strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)},
	})
	manager.MarkResult(ctx, Result{AuthID: "a", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	if manager.DrainFlags().Has("a") {
		t.Fatal("flag kept after the headers showed the longest window used up")
	}
	assertStoredFlags(t, path, map[string]bool{"a": false})
}

func TestDrainOutranksPriorityTiersAndRankMatchesPick(t *testing.T) {
	t.Run("pace", func(t *testing.T) { testDrainOutranksPriorityTiers(t, paceSelectorFor) })
	t.Run("session affinity", func(t *testing.T) {
		testDrainOutranksPriorityTiers(t, func(cache *quota.Cache) Selector { return NewSessionAffinitySelector(paceSelectorFor(cache)) })
	})
}

func testDrainOutranksPriorityTiers(t *testing.T, selector func(*quota.Cache) Selector) {
	low := map[string]string{"priority": "-1"}
	manager, cache, _ := newDrainManagerOf(t, selector,
		&Auth{ID: "a", Provider: "claude", Status: StatusActive},
		&Auth{ID: "b", Provider: "claude", Status: StatusActive, Attributes: low},
		&Auth{ID: "c", Provider: "claude", Status: StatusActive, Attributes: low},
		&Auth{ID: "d", Provider: "claude", Status: StatusActive, Attributes: low},
	)
	cache.PutProbe("a", []quota.Window{liveWindow(quota.SevenDay, 0.9, 24*time.Hour)})
	cache.PutProbe("b", []quota.Window{liveWindow(quota.SevenDay, 0.6, 84*time.Hour)})
	cache.PutProbe("c", []quota.Window{liveWindow(quota.SevenDay, 0.4, 84*time.Hour)})
	cache.PutProbe("d", []quota.Window{liveWindow(quota.SevenDay, 0.2, 84*time.Hour), {ID: quota.FiveHour, Remaining: 0, ResetAt: time.Now().Add(time.Hour), Length: fiveHour, ObservedAt: time.Now()}})
	const model = "claude-sonnet-4-5"

	for _, step := range []struct {
		flag  string
		drain bool
		want  string
	}{
		{"", false, "a"},  // nothing flagged: the top tier
		{"b", true, "b"},  // a flagged account in a lower tier wins
		{"c", true, "c"},  // c holds less of its week than b
		{"d", true, "c"},  // d's short window is spent
		{"b", false, "c"}, // c still serves
		{"c", false, "a"}, // only the spent d is flagged: routing as without the feature
	} {
		if step.flag != "" {
			if err := manager.DrainFlags().Set(step.flag, step.drain, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		ranking := manager.RankCandidates(context.Background(), "claude", model)
		first := ranking.Entries[0]
		if first.Auth.ID != step.want || !first.TopBand || first.Drain != manager.DrainFlags().Has(step.want) {
			t.Fatalf("after %s=%v: ranking leads with %+v; want %s", step.flag, step.drain, first, step.want)
		}
		for _, entry := range ranking.Entries[1:] {
			if entry.TopBand {
				t.Fatalf("after %s=%v: %s also in the top band", step.flag, step.drain, entry.Auth.ID)
			}
		}
		picked, _, err := manager.pickNext(context.Background(), "claude", model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext: %v", err)
		}
		if picked.ID != step.want {
			t.Fatalf("after %s=%v: picked %s; want %s as ranked", step.flag, step.drain, picked.ID, step.want)
		}
	}
	if d := manager.RankCandidates(context.Background(), "claude", model).Entries; !entryByID(t, d, "d").Drain || entryByID(t, d, "b").Drain {
		t.Fatalf("drain fields = %+v; want only d flagged", d)
	}
}

func TestDrainRunsTheRequestAtFullSpeed(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, shortScenario)
	id := manager.List()[0].ID
	// The flagged account sits below a higher tier, which is spent.
	registerPaceAccount(t, manager, &Auth{ID: "top", Provider: "claude", Status: StatusActive, Attributes: map[string]string{"priority": "1"}}, "claude-opus-5-5")
	manager.selector.(*PaceSelector).view.(*quota.Cache).PutProbe("top", []quota.Window{liveWindow(quota.SevenDay, 0, 84*time.Hour)})
	if err := manager.DrainFlags().Set(id, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v; want none while a flagged account serves", recorder.waits)
	}
	runway := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if runway == nil || runway.Delay != 0 || runway.ComputedDelay <= 0 {
		t.Fatalf("runway = %+v; want no delay over a computed one", runway)
	}

	if err := manager.DrainFlags().Set(id, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.waits) != 1 {
		t.Fatalf("waits = %v; want one once the flag is cleared", recorder.waits)
	}
}

func TestSessionMovesToAFlaggedAccountOnlyAtAFreePoint(t *testing.T) {
	f := newFreeMoveFixture(t)
	f.auths[0].Attributes = map[string]string{"priority": "1"}
	f.setPace("a", 0.9)
	f.setPace("b", 0.2)
	if got := f.pick(t, "task one"); got != "a" {
		t.Fatalf("first pick = %s, want a", got)
	}
	// b drains from a lower tier.
	f.draining = drainSet("b")
	if got := f.pick(t, "task one"); got != "a" {
		t.Fatalf("warm cache pick = %s, want the bound account a", got)
	}
	if got := f.pick(t, "summary of task one"); got != "b" {
		t.Fatalf("after compaction = %s, want the flagged b", got)
	}
}

func TestDrainFlagsSurviveALoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drain-flags.json")
	flags, err := LoadDrainFlags(path)
	if err != nil {
		t.Fatalf("load without a file: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := flags.Set(id, true, paceNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := flags.Set("b", false, paceNow); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("never-flagged", false, paceNow); err != nil {
		t.Fatal(err)
	}
	assertStoredFlags(t, path, map[string]bool{"a": true, "b": false, "never-flagged": false})
}

func TestLoadDrainFlagsRejectsABadFile(t *testing.T) {
	for name, content := range map[string]string{
		"version": `{"version":2,"flags":{}}`,
		"json":    `{`,
	} {
		path := filepath.Join(t.TempDir(), "drain-flags.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDrainFlags(path); err == nil {
			t.Errorf("%s: load succeeded", name)
		}
	}
}
