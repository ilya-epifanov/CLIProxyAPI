package auth

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestThrottleDelayCurve(t *testing.T) {
	on := ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}
	for _, tc := range []struct {
		name   string
		config ThrottleConfig
		pace   float64
		want   time.Duration
	}{
		{"off", ThrottleConfig{Threshold: 0.8, Floor: 0.05}, 0.1, 0},
		{"no data", on, math.NaN(), 0},
		{"ahead of the threshold", on, 1.4, 0},
		{"at the threshold", on, 0.8, 0},
		{"halfway to the floor", on, 0.425, 60 * time.Second},
		{"at the floor", on, 0.05, 120 * time.Second},
		{"below the floor", on, 0.01, 120 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.Delay(tc.pace); (got - tc.want).Abs() > time.Millisecond {
				t.Fatalf("Delay(%v) = %s, want %s", tc.pace, got, tc.want)
			}
		})
	}
}

func TestPoolPaceSumsTheUsableAccountsOnEveryWindow(t *testing.T) {
	f := newPaceFixture(t)
	// Five-hour: (0.5 + 0.1) / (0.5 + 0.5) = 0.6. Seven-day: (0.3 + 0.5) / (0.5 + 0.5) = 0.8.
	f.put("a", paceWindow(quota.FiveHour, 0.5, fiveHour/2, fiveHour), paceWindow(quota.SevenDay, 0.3, week/2, week))
	f.put("b", paceWindow(quota.FiveHour, 0.1, fiveHour/2, fiveHour), paceWindow(quota.SevenDay, 0.5, week/2, week))
	// Used up: it cannot serve, so it says nothing about how fast the rest burns.
	f.put("used-up", paceWindow(quota.FiveHour, 1, noReset, fiveHour), paceWindow(quota.SevenDay, 0, week/2, week))
	// No seven-day observation: no evidence.
	f.put("partial", paceWindow(quota.FiveHour, 0, fiveHour/2, fiveHour))
	auths := []*Auth{paceAuth("a", "claude", 0), paceAuth("b", "claude", 0), paceAuth("used-up", "claude", 0), paceAuth("partial", "claude", 0), paceAuth("no-data", "claude", 0)}

	pool := f.selector.PoolPace(context.Background(), auths, "claude-opus-5-5", paceNow)
	if math.Abs(pool.Pace-0.6) > 1e-9 || pool.Window != quota.FiveHour || pool.Accounts != 2 {
		t.Fatalf("pool = %+v; want pace 0.6 on five-hour over 2 accounts", pool)
	}
}

func TestPoolPaceAddsTheFableWindowForFableModels(t *testing.T) {
	f := newPaceFixture(t)
	f.put("a",
		paceWindow(quota.FiveHour, 1, noReset, fiveHour),
		paceWindow(quota.SevenDay, 0.45, week/2, week),
		paceWindow(quota.SevenDayFable, 0.2, week/2, week))
	auths := []*Auth{paceAuth("a", "claude", 0)}

	if pool := f.selector.PoolPace(context.Background(), auths, "claude-opus-5-5", paceNow); pool.Window != quota.SevenDay {
		t.Fatalf("non-Fable pool = %+v; want seven-day", pool)
	}
	if pool := f.selector.PoolPace(context.Background(), auths, "claude-fable-5-1", paceNow); pool.Window != quota.SevenDayFable || math.Abs(pool.Pace-0.4) > 1e-9 {
		t.Fatalf("Fable pool = %+v; want 0.4 on seven-day-fable", pool)
	}
}

func TestPoolPaceWithoutDataIsNaN(t *testing.T) {
	f := newPaceFixture(t)
	pool := f.selector.PoolPace(context.Background(), []*Auth{paceAuth("a", "claude", 0)}, "claude-opus-5-5", paceNow)
	if !math.IsNaN(pool.Pace) || pool.Accounts != 0 {
		t.Fatalf("pool = %+v; want NaN over 0 accounts", pool)
	}
}

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

// newThrottledManager registers one Claude account with a seven-day window at
// remaining, half the week left, behind a pace selector with throttle.
func newThrottledManager(t *testing.T, selector Selector, cache *quota.Cache, remaining float64) (*Manager, *throttleRecorder) {
	t.Helper()
	recorder := &throttleRecorder{}
	manager := NewManager(nil, selector, nil)
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
	cache.PutProbe(auth.ID, []quota.Window{{ID: quota.SevenDay, Remaining: remaining, ResetAt: now.Add(week / 2), Length: week, ObservedAt: now, Source: quota.SourceProbe}})
	return manager, recorder
}

func TestExecuteWaitsOnceForTheThrottleBeforeTheUpstreamCall(t *testing.T) {
	cache := quota.NewCache()
	throttle := ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}
	// Remaining 0.2125 over half the week left is pace 0.425, halfway to the floor: 60 s.
	manager, recorder := newThrottledManager(t, NewPaceSelector(PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), cache, 0.2125)

	if _, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.events) != 2 || recorder.events[0] != "wait" || recorder.events[1] != "execute" {
		t.Fatalf("events = %v; want [wait execute]", recorder.events)
	}
	if got := recorder.waits[0]; (got - 60*time.Second).Abs() > time.Second {
		t.Fatalf("wait = %s; want about 60s", got)
	}
}

func TestExecuteReturnsTheClientsCancellationDuringTheThrottle(t *testing.T) {
	cache := quota.NewCache()
	throttle := ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}
	manager, recorder := newThrottledManager(t, NewPaceSelector(PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), cache, 0.1)
	recorder.err = context.Canceled

	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v; want context.Canceled", err)
	}
	if len(recorder.events) != 1 || recorder.events[0] != "wait" {
		t.Fatalf("events = %v; the upstream call must not happen", recorder.events)
	}
}

func TestExecuteDoesNotWaitWhenThePoolIsOnScheduleOrThrottleIsOff(t *testing.T) {
	on := ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}
	for _, tc := range []struct {
		name      string
		selector  func(*quota.Cache) Selector
		remaining float64
	}{
		{"on schedule", func(c *quota.Cache) Selector { return NewPaceSelector(PaceConfig{Throttle: on}, c) }, 0.45},
		{"throttle off", func(c *quota.Cache) Selector { return NewPaceSelector(PaceConfig{}, c) }, 0.05},
		{"other strategy", func(*quota.Cache) Selector { return &RoundRobinSelector{} }, 0.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := quota.NewCache()
			manager, recorder := newThrottledManager(t, tc.selector(cache), cache, tc.remaining)
			if _, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(recorder.waits) != 0 {
				t.Fatalf("waits = %v; want none", recorder.waits)
			}
		})
	}
}

func TestExecuteStreamWaitsForTheThrottleToo(t *testing.T) {
	cache := quota.NewCache()
	throttle := ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}
	manager, recorder := newThrottledManager(t, NewPaceSelector(PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), cache, 0.1)
	recorder.err = context.Canceled

	_, err := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || len(recorder.waits) != 1 {
		t.Fatalf("ExecuteStream error = %v, waits = %v; want context.Canceled after one wait", err, recorder.waits)
	}
}
