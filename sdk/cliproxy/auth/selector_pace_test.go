package auth

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

var paceNow = time.Date(2026, 9, 11, 20, 50, 0, 0, time.UTC)

const (
	week     = 168 * time.Hour
	fiveHour = 5 * time.Hour
	// noReset marks a window with no pending reset in paceWindow.
	noReset = time.Duration(math.MinInt64)
)

type paceFixture struct {
	selector  *PaceSelector
	cache     *quota.Cache
	refreshed []string
	intnArg   int
}

func newPaceFixture(t *testing.T) *paceFixture {
	t.Helper()
	f := &paceFixture{cache: quota.NewCache()}
	f.cache.SetRefreshRequester(func(authID string) { f.refreshed = append(f.refreshed, authID) })
	f.selector = NewPaceSelector(PaceConfig{BurstReserve: 0.2, PaceTolerance: 0.1, StaleAfter: 15 * time.Minute}, f.cache)
	f.selector.now = func() time.Time { return paceNow }
	f.selector.intn = func(n int) int {
		f.intnArg = n
		return 0
	}
	return f
}

// paceWindow builds a probe window resetting resetIn from paceNow (noReset: none pending).
func paceWindow(id quota.WindowID, remaining float64, resetIn, length time.Duration) quota.Window {
	w := quota.Window{ID: id, Remaining: remaining, Length: length, ObservedAt: paceNow, Source: quota.SourceProbe}
	if resetIn != noReset {
		w.ResetAt = paceNow.Add(resetIn)
	}
	return w
}

func (f *paceFixture) put(authID string, windows ...quota.Window) {
	f.cache.PutProbe(authID, windows)
}

func paceAuth(id, provider string, priority int) *Auth {
	return &Auth{ID: id, Provider: provider, Attributes: map[string]string{"priority": strconv.Itoa(priority)}}
}

func rankIDs(ranked []Ranked) []string {
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.Auth.ID
	}
	return out
}

func entryByID(t *testing.T, ranked []Ranked, id string) Ranked {
	t.Helper()
	for _, r := range ranked {
		if r.Auth.ID == id {
			return r
		}
	}
	t.Fatalf("no entry %s in %v", id, rankIDs(ranked))
	return Ranked{}
}

func assertOrder(t *testing.T, ranked []Ranked, want ...string) {
	t.Helper()
	got := rankIDs(ranked)
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func assertNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestPaceSelectorRanksBehindScheduleFirst(t *testing.T) {
	f := newPaceFixture(t)
	// A: 10 % left, 8 h to reset → pace 2.1. B: 60 %, 84 h → 1.2. R−T would prefer B.
	f.put("a", paceWindow(quota.SevenDay, 0.10, 8*time.Hour, week))
	f.put("b", paceWindow(quota.SevenDay, 0.60, 84*time.Hour, week))
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("b", "claude", 0), paceAuth("a", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "a", "b")
	assertNear(t, "pace a", ranked[0].Pace, 2.1)
	assertNear(t, "pace b", ranked[1].Pace, 1.2)
	if ranked[0].PaceWindow != quota.SevenDay || ranked[0].Class != ClassReady {
		t.Fatalf("entry a = %+v", ranked[0])
	}
	if !ranked[0].TopBand || ranked[1].TopBand {
		t.Fatalf("top band = %v, %v; want only a", ranked[0].TopBand, ranked[1].TopBand)
	}
}

func TestPaceSelectorPicksWithinTopBand(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.625, 84*time.Hour, week)) // 1.25
	f.put("b", paceWindow(quota.SevenDay, 0.60, 84*time.Hour, week))  // 1.20, within 10 %
	f.put("c", paceWindow(quota.SevenDay, 0.45, 84*time.Hour, week))  // 0.90
	auths := []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0), paceAuth("c", "claude", 0)}

	ranked := f.selector.Rank(context.Background(), auths, "", paceNow)
	assertOrder(t, ranked, "a", "b", "c")
	if !ranked[0].TopBand || !ranked[1].TopBand || ranked[2].TopBand {
		t.Fatalf("top band = %v %v %v; want a, b", ranked[0].TopBand, ranked[1].TopBand, ranked[2].TopBand)
	}

	picked, err := f.selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if f.intnArg != 2 || picked.ID != "a" {
		t.Fatalf("Pick chose index 0 of %d = %s; want a of 2", f.intnArg, picked.ID)
	}
}

func TestPaceSelectorBurstReserve(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.4, 33600*time.Second, week), paceWindow(quota.FiveHour, 0.10, 2*time.Hour, fiveHour)) // pace 2.0
	f.put("b", paceWindow(quota.SevenDay, 0.55, 84*time.Hour, week), paceWindow(quota.FiveHour, 0.80, 2*time.Hour, fiveHour))
	// c: 5-hour low but its reset passed, so it refilled.
	f.put("c", paceWindow(quota.SevenDay, 0.30, 84*time.Hour, week), paceWindow(quota.FiveHour, 0.05, -time.Minute, fiveHour))
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0), paceAuth("c", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "b", "c", "a")
	if a := entryByID(t, ranked, "a"); a.Class != ClassBurstLow || a.ShortRemaining != 0.10 {
		t.Fatalf("a = %+v, want burst low with short 0.10", a)
	}
	if c := entryByID(t, ranked, "c"); c.Class != ClassReady {
		t.Fatalf("c class = %v, want ready", c.Class)
	}
}

func TestPaceSelectorOrdersBurstLowByPace(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.4, 33600*time.Second, week), paceWindow(quota.FiveHour, 0.10, time.Hour, fiveHour))
	f.put("b", paceWindow(quota.SevenDay, 0.5, 84*time.Hour, week), paceWindow(quota.FiveHour, 0.15, time.Hour, fiveHour))
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("b", "claude", 0), paceAuth("a", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "a", "b")
	if !ranked[0].TopBand || ranked[0].Class != ClassBurstLow {
		t.Fatalf("rank 1 = %+v, want burst low top band", ranked[0])
	}
}

func TestPaceSelectorFableModel(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a",
		paceWindow(quota.SevenDay, 0.75, 84*time.Hour, week),      // 1.5
		paceWindow(quota.SevenDayFable, 0.20, 84*time.Hour, week), // 0.4
	)
	f.put("b",
		paceWindow(quota.SevenDay, 0.5, 84*time.Hour, week),
		paceWindow(quota.SevenDayFable, 0.5, 84*time.Hour, week),
	)
	auths := []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}

	fable := f.selector.Rank(context.Background(), auths, "claude-fable-5-1", paceNow)
	assertOrder(t, fable, "b", "a")
	a := entryByID(t, fable, "a")
	if a.PaceWindow != quota.SevenDayFable || len(a.LongWindows) != 2 {
		t.Fatalf("a for Fable = %+v, want pace window seven-day-fable and two long windows", a)
	}

	other := f.selector.Rank(context.Background(), auths, "claude-sonnet-4-5", paceNow)
	assertOrder(t, other, "a", "b")
	if a := entryByID(t, other, "a"); len(a.LongWindows) != 1 || a.LongWindows[0] != quota.SevenDay {
		t.Fatalf("a long windows for Sonnet = %v, want [seven-day]", a.LongWindows)
	}
}

func TestPaceSelectorFableExhaustedOnlyForFable(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.8, 84*time.Hour, week), paceWindow(quota.SevenDayFable, 0, 20*time.Hour, week))
	f.put("b", paceWindow(quota.SevenDay, 0.2, 84*time.Hour, week), paceWindow(quota.SevenDayFable, 0.2, 84*time.Hour, week))
	auths := []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}

	fable := f.selector.Rank(context.Background(), auths, "claude-fable-5-1(high)", paceNow)
	assertOrder(t, fable, "b", "a")
	if a := entryByID(t, fable, "a"); a.Class != ClassExhausted || a.ExhaustedWindow != quota.SevenDayFable || !a.BackAt.Equal(paceNow.Add(20*time.Hour)) {
		t.Fatalf("a for Fable = %+v", a)
	}
	other := f.selector.Rank(context.Background(), auths, "claude-sonnet-4-5", paceNow)
	assertOrder(t, other, "a", "b")
}

func TestPaceSelectorUsesSelectionModelFromContext(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.8, 84*time.Hour, week), paceWindow(quota.SevenDayFable, 0, 20*time.Hour, week))
	auths := []*Auth{paceAuth("a", "claude", 0)}

	// Route model "fast" is an alias of a Fable model on account a.
	ctx := withSelectionModels(context.Background(), map[string]string{"a": "claude-fable-5-1"})
	if a := f.selector.Rank(ctx, auths, "fast", paceNow)[0]; a.Class != ClassExhausted {
		t.Fatalf("with selection model: class = %v, want exhausted", a.Class)
	}
	// Without the map the route model is used, and "fast" is not a Fable model.
	if a := f.selector.Rank(context.Background(), auths, "fast", paceNow)[0]; a.Class != ClassReady {
		t.Fatalf("without selection model: class = %v, want ready", a.Class)
	}
}

func TestPaceSelectorExhaustedShortWindow(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.9, 24*time.Hour, week), paceWindow(quota.FiveHour, 0, 2*time.Hour, fiveHour))
	f.put("b", paceWindow(quota.SevenDay, 0.1, 160*time.Hour, week))
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "b", "a")
	if a := ranked[1]; a.Class != ClassExhausted || a.ExhaustedWindow != quota.FiveHour {
		t.Fatalf("a = %+v, want exhausted on five-hour", a)
	}
}

func TestPaceSelectorResetPassedIsEstimatedAndRefreshes(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.1, -time.Hour, week))
	f.put("b", paceWindow(quota.SevenDay, 0.4, 84*time.Hour, week)) // 0.8
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "a", "b")
	if a := ranked[0]; a.Pace != 1 || !a.Estimated {
		t.Fatalf("a = %+v, want pace 1 estimated", a)
	}
	if len(f.refreshed) != 1 || f.refreshed[0] != "a" {
		t.Fatalf("refresh requests = %v, want [a]", f.refreshed)
	}
}

func TestPaceSelectorPaceEdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		window quota.Window
		want   float64
	}{
		{"idle window", paceWindow(quota.SevenDay, 1, noReset, week), 1},
		{"no reset reported", paceWindow(quota.SevenDay, 0.6, noReset, week), 0.6},
		{"just before reset", paceWindow(quota.SevenDay, 0.01, 10*time.Second, week), 0.01 / (float64(time.Minute) / float64(week))},
		{"reset beyond window length", paceWindow(quota.SevenDay, 0.5, 2*week, week), 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, estimated := pace(tt.window, paceNow)
			if estimated || math.IsInf(got, 0) {
				t.Fatalf("pace = %v estimated=%v", got, estimated)
			}
			assertNear(t, "pace", got, tt.want)
		})
	}
}

func TestPaceSelectorUnknownAccounts(t *testing.T) {
	f := newPaceFixture(t)
	f.put("b", paceWindow(quota.SevenDay, 0.15, 84*time.Hour, week)) // 0.3
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "b", "a")
	if ranked[1].Class != ClassUnknown || ranked[1].TopBand {
		t.Fatalf("a = %+v, want unknown outside the top band", ranked[1])
	}

	empty := newPaceFixture(t)
	auths := []*Auth{paceAuth("c", "claude", 0), paceAuth("a", "claude", 0), paceAuth("b", "xai", 0)}
	all := empty.selector.Rank(context.Background(), auths, "", paceNow)
	assertOrder(t, all, "a", "b", "c")
	for _, r := range all {
		if r.Class != ClassUnknown || !r.TopBand {
			t.Fatalf("%s = %+v, want unknown in the top band", r.Auth.ID, r)
		}
	}
}

func TestPaceSelectorFableWindowMissingIsUnknown(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0.5, 84*time.Hour, week))
	a := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0)}, "claude-fable-5-1", paceNow)[0]
	if a.Class != ClassUnknown {
		t.Fatalf("class = %v, want unknown when the Fable window was never probed", a.Class)
	}
}

func TestPaceSelectorExhaustedOrder(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.SevenDay, 0, 72*time.Hour, week))
	f.put("b", paceWindow(quota.SevenDay, 0, 48*time.Hour, week))
	f.put("c", paceWindow(quota.SevenDay, 0, noReset, week))
	auths := []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0), paceAuth("c", "claude", 0)}
	ranked := f.selector.Rank(context.Background(), auths, "", paceNow)
	assertOrder(t, ranked, "b", "a", "c")
	if !ranked[0].TopBand || ranked[1].TopBand || ranked[2].TopBand {
		t.Fatal("only the first exhausted entry may be in the top band")
	}
}

func TestPaceSelectorStale(t *testing.T) {
	f := newPaceFixture(t)
	old := paceWindow(quota.SevenDay, 0.7, 84*time.Hour, week)
	old.ObservedAt = paceNow.Add(-20 * time.Minute)
	f.put("a", old)
	f.put("b", paceWindow(quota.SevenDay, 0.5, 84*time.Hour, week))
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0)}, "", paceNow)
	assertOrder(t, ranked, "a", "b")
	if !ranked[0].Stale || ranked[1].Stale {
		t.Fatalf("stale = %v, %v; want a only", ranked[0].Stale, ranked[1].Stale)
	}
}

func TestPaceSelectorCodexMonthlySlot(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a", paceWindow(quota.Monthly, 0.5, 15*24*time.Hour, 30*24*time.Hour))
	a := f.selector.Rank(context.Background(), []*Auth{paceAuth("a", "codex", 0)}, "gpt-5-codex", paceNow)[0]
	if a.Class != ClassReady || a.PaceWindow != quota.Monthly {
		t.Fatalf("a = %+v, want ready on monthly", a)
	}
	assertNear(t, "pace", a.Pace, 1)
}

func TestPaceSelectorMixedProvidersUseOwnWindows(t *testing.T) {
	f := newPaceFixture(t)
	f.put("claude-a", paceWindow(quota.SevenDay, 0.6, 84*time.Hour, week)) // 1.2
	f.put("codex-b", paceWindow(quota.Weekly, 0.75, 84*time.Hour, week))   // 1.5
	ranked := f.selector.Rank(context.Background(), []*Auth{paceAuth("claude-a", "claude", 0), paceAuth("codex-b", "codex", 0)}, "", paceNow)
	assertOrder(t, ranked, "codex-b", "claude-a")
}

func TestPaceSelectorPickHonoursTiersAndCooldowns(t *testing.T) {
	f := newPaceFixture(t)
	f.put("pinned", paceWindow(quota.SevenDay, 0.1, 84*time.Hour, week)) // 0.2
	f.put("normal", paceWindow(quota.SevenDay, 1, 84*time.Hour, week))   // 2.0
	f.put("cooling", paceWindow(quota.SevenDay, 0.1, time.Hour, week))
	// Pick checks cooldowns against the wall clock, like the built-in selectors.
	cooling := paceAuth("cooling", "claude", 1)
	cooling.Unavailable = true
	cooling.NextRetryAfter = time.Now().Add(5 * time.Minute)

	picked, err := f.selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{},
		[]*Auth{paceAuth("normal", "claude", 0), paceAuth("pinned", "claude", 1), cooling})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if picked.ID != "pinned" {
		t.Fatalf("picked %s, want pinned (highest available tier)", picked.ID)
	}
}

func TestRankClassString(t *testing.T) {
	for class, want := range map[RankClass]string{ClassReady: "ready", ClassBurstLow: "burst_low", ClassUnknown: "unknown", ClassExhausted: "exhausted"} {
		if got := class.String(); got != want {
			t.Fatalf("%d.String() = %q, want %q", class, got, want)
		}
	}
}
