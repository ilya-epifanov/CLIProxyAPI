package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

type throttleRecorder struct {
	events []string
	waits  []time.Duration
	err    error
}

type throttleTestExecutor struct {
	schedulerProviderTestExecutor
	recorder *throttleRecorder
}

func (e throttleTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.recorder.events = append(e.recorder.events, "execute")
	return cliproxyexecutor.Response{}, nil
}

// throttleScenario is one Claude account and its recent use, all relative to now.
type throttleScenario struct {
	// lastHour is spread over the last hour as one session with 10 s turns.
	lastHour float64
	// earlier is spent two days ago, inside the 7-day window but outside the 5-hour one.
	earlier float64
	// fiveHour and sevenDay are the remaining fractions; the 5-hour window started 2.5 h
	// ago, the 7-day one 3.5 days ago.
	fiveHour, sevenDay float64
}

// newThrottledManager registers one Claude account behind a pace selector with throttle
// and fills the ledger and the quota view with scenario.
func newThrottledManager(t *testing.T, throttle ThrottleConfig, sc throttleScenario) (*Manager, *throttleRecorder) {
	t.Helper()
	cache := quota.NewCache()
	recorder := &throttleRecorder{}
	manager := NewManager(nil, NewPaceSelector(PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), nil)
	manager.RegisterExecutor(throttleTestExecutor{schedulerProviderTestExecutor{provider: "claude"}, recorder})
	manager.throttleWait = func(_ context.Context, d time.Duration) error {
		recorder.events = append(recorder.events, "wait")
		recorder.waits = append(recorder.waits, d)
		return recorder.err
	}
	auth := &Auth{ID: "throttle-" + t.Name(), Provider: "claude", Status: StatusActive}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	now := time.Now()
	manager.ledger = NewUsageLedger(now.Add(-8 * 24 * time.Hour))
	index := auth.EnsureIndex()
	add := func(at time.Time, units float64) {
		manager.ledger.Add(LedgerEntry{At: at, Provider: "claude", AuthIndex: index, Model: "claude-opus-5-5", SessionID: "s1", Units: units}, false)
	}
	add(now.Add(-48*time.Hour), sc.earlier)
	const turns = 360
	for i := range turns {
		add(now.Add(-time.Hour+time.Duration(i)*10*time.Second), sc.lastHour/turns)
	}
	cache.PutProbe(auth.ID, []quota.Window{
		{ID: quota.FiveHour, Remaining: sc.fiveHour, ResetAt: now.Add(150 * time.Minute), Length: fiveHour, ObservedAt: now, Source: quota.SourceProbe},
		{ID: quota.SevenDay, Remaining: sc.sevenDay, ResetAt: now.Add(84 * time.Hour), Length: week, ObservedAt: now, Source: quota.SourceProbe},
	})
	return manager, recorder
}

var (
	throttleOn = ThrottleConfig{MaxDelay: 120 * time.Second, Margin: 0.15}
	// clearScenario: 20 units in the last hour of a 200-unit 5-hour window, 800-unit week.
	clearScenario = throttleScenario{lastHour: 20, earlier: 300, fiveHour: 0.9, sevenDay: 0.6}
	// shortScenario: 100 units in the last hour leave 100 of the 5-hour window, which the
	// next hour's 115 units overrun.
	shortScenario = throttleScenario{lastHour: 100, earlier: 300, fiveHour: 0.5, sevenDay: 0.5}
)

func execute(t *testing.T, manager *Manager) error {
	t.Helper()
	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
	return err
}

func TestRunwayIsClearWhenThePoolLasts(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, clearScenario)
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v; want none", recorder.waits)
	}
	ranking := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5")
	if r := ranking.Runway; r == nil || r.Reason != RunwayClear || r.Speed != 1 || r.Window != quota.SevenDay || r.Accounts != 1 {
		t.Fatalf("runway = %+v; want clear on seven-day over 1 account", r)
	}
}

func TestExecuteWaitsOnceSoThePoolDoesNotRunDry(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, shortScenario)
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.events) != 2 || recorder.events[0] != "wait" || recorder.events[1] != "execute" {
		t.Fatalf("events = %v; want [wait execute]", recorder.events)
	}
	runway := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if runway == nil || runway.Reason != RunwayShort || runway.Speed <= 0 || runway.Speed >= 1 || runway.DryAt.IsZero() {
		t.Fatalf("runway = %+v; want short with a speed in (0, 1)", runway)
	}
	// 10 s turns at speed s wait 10 s × (1/s − 1).
	want := time.Duration(float64(10*time.Second) * (1/runway.Speed - 1))
	if got := recorder.waits[0]; got <= 0 || got >= throttleOn.MaxDelay || (got-want).Abs() > time.Second {
		t.Fatalf("wait = %s; want about %s, under max-delay", got, want)
	}
}

func TestExecuteSlowsSoTheBurnLastsUntilTheRefill(t *testing.T) {
	guarded := throttleOn
	guarded.GuardHorizon = 12 * time.Hour
	manager, recorder := newThrottledManager(t, guarded, shortScenario)
	// 40 min ago the 5-hour window was 30 % used, now 50 %: 0.3 an hour, 0.345 with the
	// margin, so the 50 % left lasts 1.45 h; it refills in 2.5 h.
	auth := manager.auths["throttle-"+t.Name()]
	manager.ledger.Observe(auth.EnsureIndex(), quota.Window{ID: quota.FiveHour, Remaining: 0.7, ResetAt: time.Now().Add(150 * time.Minute), Length: fiveHour, ObservedAt: time.Now().Add(-40 * time.Minute)})
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if want := 0.5 / 0.345 / 2.5; r == nil || r.Reason != RunwayReserve || math.Abs(r.Speed-want) > 0.01 {
		t.Fatalf("runway = %+v; want the guard at about %.2f", r, want)
	}
	want := time.Duration(float64(10*time.Second) * (1/r.Speed - 1))
	if len(recorder.waits) != 1 || (recorder.waits[0]-want).Abs() > time.Second {
		t.Fatalf("waits = %v; want one of about %s", recorder.waits, want)
	}
}

func TestGuardSpeed(t *testing.T) {
	refill := func(in time.Duration) time.Time { return paceNow.Add(in) }
	for _, tc := range []struct {
		name    string
		guard   map[quota.WindowID]*guardWindow
		horizon time.Duration
		speed   float64
		under   bool
	}{
		{"off", map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.1, burn: 1, refill: refill(time.Hour)}}, 0, 0, false},
		{"no burn", map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.1, refill: refill(time.Hour)}}, 12 * time.Hour, 1, false},
		// 0.5 at 0.25 an hour lasts 2 h; the refill is in 1 h.
		{"lasts until the refill", map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.5, burn: 0.25, refill: refill(time.Hour)}}, 12 * time.Hour, 1, false},
		// The same room must last 4 h: half the burn.
		{"refill later", map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.5, burn: 0.25, refill: refill(4 * time.Hour)}}, 12 * time.Hour, 0.5, true},
		// The 7-day refill is days away; the horizon caps it at 12 h.
		{"horizon", map[quota.WindowID]*guardWindow{quota.SevenDay: {room: 0.06, burn: 0.01, refill: refill(80 * time.Hour)}}, 12 * time.Hour, 0.5, true},
		{"no refill pending", map[quota.WindowID]*guardWindow{quota.SevenDay: {room: 0.06, burn: 0.01}}, 12 * time.Hour, 0.5, true},
		{"empty", map[quota.WindowID]*guardWindow{quota.FiveHour: {burn: 0.2, refill: refill(time.Hour)}}, 12 * time.Hour, 0, true},
		{"tightest window wins", map[quota.WindowID]*guardWindow{
			quota.FiveHour: {room: 0.5, burn: 0.25, refill: refill(4 * time.Hour)},
			quota.SevenDay: {room: 0.03, burn: 0.01, refill: refill(80 * time.Hour)},
		}, 12 * time.Hour, 0.25, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			speed, under := sizedPool{guard: tc.guard}.guardSpeed(tc.horizon, 0, 0, 1, paceNow)
			if under != tc.under || (under && math.Abs(speed-tc.speed) > 1e-9) {
				t.Fatalf("guardSpeed = %v, %v; want %v, %v", speed, under, tc.speed, tc.under)
			}
		})
	}
	// The reserve is held back: 0.5 less 0.1 at 0.25 an hour lasts 1.6 h of the 4 h.
	reserved := sizedPool{guard: map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.5, burn: 0.25, refill: refill(4 * time.Hour)}}}
	if speed, _ := reserved.guardSpeed(12*time.Hour, 0, 0.1, 1, paceNow); math.Abs(speed-0.4) > 1e-9 {
		t.Fatalf("reserved speed = %v; want 0.4", speed)
	}
	// The burn is undone by the speed it ran at and inflated by the margin.
	pool := sizedPool{guard: map[quota.WindowID]*guardWindow{quota.FiveHour: {room: 0.5, burn: 0.125, refill: refill(4 * time.Hour)}}}
	if speed, _ := pool.guardSpeed(12*time.Hour, 0.25, 0, 0.5, paceNow); math.Abs(speed-0.4) > 1e-9 {
		t.Fatalf("speed = %v; want 0.5 / (0.125 / 0.5 × 1.25) / 4 = 0.4", speed)
	}
}

func TestGuardIgnoresAGlobalSizeError(t *testing.T) {
	// Every account sized twice too large: room and burn both count in median sizes, so
	// the guard does not change.
	window := func(units float64) RunwayWindow {
		return RunwayWindow{ID: quota.FiveHour, Remaining: 0.4 * units, Capacity: units, ResetAt: paceNow.Add(3 * time.Hour), Length: fiveHour}
	}
	speeds := []float64{}
	for _, scale := range []float64{1, 2} {
		var g *guardWindow
		for _, units := range []float64{100, 200} {
			sized := units * scale
			g = g.add(window(sized), sized/(150*scale), 0.2, true)
		}
		speed, _ := sizedPool{guard: map[quota.WindowID]*guardWindow{quota.FiveHour: g}}.guardSpeed(12*time.Hour, 0, 0, 1, paceNow)
		speeds = append(speeds, speed)
	}
	if math.Abs(speeds[0]-speeds[1]) > 1e-12 || speeds[0] >= 1 {
		t.Fatalf("speeds = %v; want the same, under 1", speeds)
	}
}

func TestExecuteDoesNotWaitWhileLearning(t *testing.T) {
	// 5 % of each window spent is too little to size either.
	learning := shortScenario
	learning.sevenDay, learning.fiveHour = 0.95, 0.95
	manager, recorder := newThrottledManager(t, throttleOn, learning)
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v; want none", recorder.waits)
	}
	if r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway; r == nil || r.Reason != RunwayLearning {
		t.Fatalf("runway = %+v; want learning", r)
	}
}

func TestRunwayPlansOnTheLongWindowsWhileTheShortOneIsUnsized(t *testing.T) {
	// 5 % of the 5-hour window spent: it is left out, and the week alone is clear.
	unsized := shortScenario
	unsized.fiveHour = 0.95
	manager, recorder := newThrottledManager(t, throttleOn, unsized)
	if err := execute(t, manager); err != nil || len(recorder.waits) != 0 {
		t.Fatalf("err = %v, waits = %v; want no wait", err, recorder.waits)
	}
	r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if r == nil || r.Reason != RunwayClear || len(r.Unsized) != 1 || r.Unsized[0] != quota.FiveHour {
		t.Fatalf("runway = %+v; want clear with five-hour unsized", r)
	}
}

func TestRunwayPlansOnTheShortWindowWhileTheLongOneIsUnsized(t *testing.T) {
	// A week barely touched but a 5-hour window half gone: the next hour still overruns it.
	unsized := shortScenario
	unsized.sevenDay = 0.95
	manager, recorder := newThrottledManager(t, throttleOn, unsized)
	if err := execute(t, manager); err != nil || len(recorder.waits) != 1 || recorder.waits[0] <= 0 {
		t.Fatalf("err = %v, waits = %v; want one wait", err, recorder.waits)
	}
	r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if r == nil || r.Reason != RunwayShort || len(r.Unsized) != 1 || r.Unsized[0] != quota.SevenDay {
		t.Fatalf("runway = %+v; want short with seven-day unsized", r)
	}
}

func TestRunwaySizesAnOutlierAsThePoolMedian(t *testing.T) {
	// One account looks four times the size of the others, as after a reset spent while
	// the gateway was down; sized as the median, the pool runs short.
	cache := quota.NewCache()
	manager := NewManager(nil, NewPaceSelector(PaceConfig{PaceTolerance: 0.1, Throttle: throttleOn}, cache), nil)
	now := time.Now()
	manager.ledger = NewUsageLedger(now.Add(-8 * 24 * time.Hour))
	for i, spent := range []float64{400, 400, 1600} {
		auth := &Auth{ID: fmt.Sprintf("outlier-%s-%d", t.Name(), i), Provider: "claude", Status: StatusActive}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		// Spent 60 h ago, after the window started 68 h ago; it is also the week's demand.
		manager.ledger.Add(LedgerEntry{At: now.Add(-60 * time.Hour), Provider: "claude", AuthIndex: auth.EnsureIndex(), Model: "claude-opus-5-5", Units: spent}, false)
		cache.PutProbe(auth.ID, []quota.Window{
			{ID: quota.FiveHour, Remaining: 1, Length: fiveHour, ObservedAt: now, Source: quota.SourceProbe},
			{ID: quota.SevenDay, Remaining: 0.5, ResetAt: now.Add(100 * time.Hour), Length: week, ObservedAt: now, Source: quota.SourceProbe},
		})
	}
	// From hour 24 the week's average, 2 400 / 168 × 1.15 ≈ 16.4 units an hour, wants
	// about 1 250 units before the refill at 100 h: more than 3 × 400 left at 800 units
	// each, less than the 2 400 left if the outlier counted as 3 200.
	r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if r == nil || r.Reason != RunwayShort || r.Speed >= 1 {
		t.Fatalf("runway = %+v; want short, sized 800 each", r)
	}
}

func TestExecuteDoesNotWaitWhenThrottleIsOffOrTheStrategyIsNotPace(t *testing.T) {
	manager, recorder := newThrottledManager(t, ThrottleConfig{}, shortScenario)
	if err := execute(t, manager); err != nil || len(recorder.waits) != 0 {
		t.Fatalf("throttle off: err = %v, waits = %v", err, recorder.waits)
	}
	if r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway; r == nil || r.Reason != RunwayShort || r.Delay != 0 {
		t.Fatalf("throttle off: runway = %+v; want short with no delay", r)
	}

	other, recorder := newThrottledManager(t, throttleOn, shortScenario)
	other.SetSelector(&RoundRobinSelector{})
	if err := execute(t, other); err != nil || len(recorder.waits) != 0 {
		t.Fatalf("round-robin: err = %v, waits = %v", err, recorder.waits)
	}
	if r := other.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway; r != nil {
		t.Fatalf("round-robin: runway = %+v; want nil", r)
	}
}

func TestExecuteReturnsTheClientsCancellationDuringTheThrottle(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, shortScenario)
	recorder.err = context.Canceled
	if err := execute(t, manager); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v; want context.Canceled", err)
	}
	if len(recorder.events) != 1 || recorder.events[0] != "wait" {
		t.Fatalf("events = %v; the upstream call must not happen", recorder.events)
	}
}

func TestExecuteStreamWaitsForTheThrottleToo(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, shortScenario)
	recorder.err = context.Canceled
	_, err := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || len(recorder.waits) != 1 {
		t.Fatalf("ExecuteStream error = %v, waits = %v; want context.Canceled after one wait", err, recorder.waits)
	}
}

func TestThrottleLogMessageCarriesTheNumbers(t *testing.T) {
	status := RunwayStatus{Reason: RunwayShort, Speed: 0.7451, Delay: 21920 * time.Millisecond, DryAt: paceNow.Add(127*time.Hour + 10*time.Second), Window: quota.SevenDay, Accounts: 5, Demand: 47.53}
	want := "pace throttle: delaying request 22s so the pool does not run dry (speed 0.75 on seven-day, 5 accounts, demand 47.5 units/h, dry in 127h0m0s at full speed)"
	if got := throttleLogMessage(status, paceNow); got != want {
		t.Fatalf("message = %q\nwant      %q", got, want)
	}
	status.Reason, status.Speed, status.DryAt = RunwayReserve, 0, time.Time{}
	want = "pace throttle: delaying request 22s so the pool does not run dry (speed 0.00 on seven-day, 5 accounts, demand 47.5 units/h)"
	if got := throttleLogMessage(status, paceNow); got != want {
		t.Fatalf("reserve message = %q\nwant              %q", got, want)
	}
}
