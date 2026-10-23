package auth

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

// ThrottleConfig slows requests down only as much as keeps the pool from running dry:
// a pool that runs dry stops every client at once with 429s, while one account that
// runs out only moves its sessions to another (docs/frontend-design.md §4.7).
type ThrottleConfig struct {
	// MaxDelay is the longest wait added to one request; 0 turns throttling off.
	MaxDelay time.Duration
	// Margin inflates the forecast demand, for error in the forecast and the capacity
	// estimates.
	Margin float64
	// Reserve: while the accounts that can serve now hold less than this long of the
	// last hour's demand, every request waits MaxDelay. 0 turns the guard off.
	Reserve time.Duration
}

// Enabled reports whether requests are ever delayed.
func (c ThrottleConfig) Enabled() bool {
	return c.MaxDelay > 0
}

// RunwayReason says what sets a pool's throttle.
type RunwayReason string

const (
	// RunwayClear: the forecast is served at full speed for a week.
	RunwayClear RunwayReason = "clear"
	// RunwayShort: at full speed the pool runs dry within a week; requests run at Speed.
	RunwayShort RunwayReason = "short"
	// RunwayReserve: the accounts that can serve now hold less than the reserve.
	RunwayReserve RunwayReason = "reserve"
	// RunwayLearning: no account has spent enough of a window to size it yet.
	RunwayLearning RunwayReason = "learning"
)

// RunwayStatus is a pool's runway and the throttle it sets.
type RunwayStatus struct {
	Reason RunwayReason
	// Speed is the share of the forecast the pool can serve for a week; 1 when clear,
	// 0 under the reserve guard, NaN while learning.
	Speed float64
	// Delay is what a request waits now; 0 when throttling is off.
	Delay time.Duration
	// DryAt is when the pool runs dry at full speed; zero when it lasts a week.
	DryAt time.Time
	// Window is the long window of the plan that limits the speed.
	Window quota.WindowID
	// Accounts is how many accounts the plan spends.
	Accounts int
	// Demand is the next hour's forecast in cost units per hour, margin included.
	Demand float64
	// Unsized lists the short windows no account has spent enough of to size yet; the
	// plan leaves them out, so it does not see their rate limit.
	Unsized []quota.WindowID
	Config  ThrottleConfig
}

// Throttle returns the selector's throttle settings.
func (s *PaceSelector) Throttle() ThrottleConfig {
	return s.cfg.Throttle
}

// paceSelectorOf returns the PaceSelector behind selector, looking through session affinity.
func paceSelectorOf(selector Selector) (*PaceSelector, bool) {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	pace, ok := selector.(*PaceSelector)
	return pace, ok && pace != nil
}

// Ledger returns the usage ledger the throttle plans with. The service registers it as
// a usage plugin and seeds it from the usage store.
func (m *Manager) Ledger() *UsageLedger {
	return m.ledger
}

// runwayLocked plans the runway of the pools behind providers for model, the tightest
// provider deciding, or ok false when the strategy is not pace. The caller holds m.mu.
func (m *Manager) runwayLocked(ctx context.Context, providers []string, model string, now time.Time) (RunwayStatus, bool) {
	pace, ok := paceSelectorOf(m.selector)
	if !ok || m.ledger == nil {
		return RunwayStatus{}, false
	}
	cfg := pace.Throttle()
	var out RunwayStatus
	for i, provider := range providers {
		status := m.providerRunwayLocked(ctx, pace, provider, model, now)
		if i == 0 || runwayTighter(status, out) {
			out = status
		}
	}
	out.Config = cfg
	if cfg.Enabled() {
		out.Delay = m.throttleDelay(out, now)
	}
	return out, true
}

// runwayTighter reports whether a throttles harder than b; learning throttles least.
func runwayTighter(a, b RunwayStatus) bool {
	if a.Reason == RunwayLearning || b.Reason == RunwayLearning {
		return b.Reason == RunwayLearning && a.Reason != RunwayLearning
	}
	return a.Speed < b.Speed
}

// throttleDelay turns a speed into a wait: an agent loop of turns cycle long runs at
// cycle / (cycle + delay). Without a turn length it waits MaxDelay.
func (m *Manager) throttleDelay(status RunwayStatus, now time.Time) time.Duration {
	cfg := status.Config
	if status.Reason == RunwayLearning || status.Speed >= 1 {
		return 0
	}
	cycle, ok := m.ledger.Cycle(now)
	if !ok || status.Speed <= 0 {
		return cfg.MaxDelay
	}
	delay := time.Duration(float64(cycle) * (1/status.Speed - 1))
	return min(delay, cfg.MaxDelay)
}

// maxSizeOverMedian bounds an account's estimated window size, as a multiple of its
// pool's median, before the median replaces it.
const maxSizeOverMedian = 1.5

// planGroup is one runway plan: the windows that cap each account, and the window whose
// demand is spent from them.
type planGroup struct {
	demand  quota.WindowID
	windows []quota.WindowID
}

// observedAccount is one account of a pool with every routing window of its model
// observed, and the sizes the ledger has for them.
type observedAccount struct {
	short   quota.WindowID
	windows map[quota.WindowID]quota.Window
	sizes   map[quota.WindowID]float64
	groups  []planGroup
	// usable: a candidate now, not cooling down.
	usable bool
}

// sizedPool is a pool ready to plan: per demand window, the accounts that spend it.
type sizedPool struct {
	order   []planGroup
	plans   map[quota.WindowID][]RunwayAccount
	reserve float64
	unsized []quota.WindowID
}

// providerRunwayLocked plans one provider's pool. The short window and the first long
// slot share all of the provider's demand, so they are planned together; a further long
// slot (the Fable window) is spent only by its own models and is planned alone.
func (m *Manager) providerRunwayLocked(ctx context.Context, pace *PaceSelector, provider, model string, now time.Time) RunwayStatus {
	pool, ok := sizePool(m.observePoolLocked(ctx, pace, provider, model, now), now)
	if !ok || len(pool.order) == 0 {
		return RunwayStatus{Reason: RunwayLearning, Speed: math.NaN()}
	}
	cfg := pace.Throttle()
	margin := 1 + cfg.Margin
	var out RunwayStatus
	for i, group := range pool.order {
		f := m.ledger.Forecast(provider, group.demand, now)
		f = Forecast{Hour: f.Hour * margin, Day: f.Day * margin, Week: f.Week * margin}
		accounts := pool.plans[group.demand]
		plan := PlanRunway(accounts, f, now)
		status := RunwayStatus{Reason: RunwayClear, Speed: plan.Speed, DryAt: plan.DryAt, Window: group.demand, Accounts: len(accounts), Demand: f.Hour, Unsized: pool.unsized}
		if plan.Speed < 1 {
			status.Reason = RunwayShort
		}
		if i == 0 && cfg.Reserve > 0 && pool.reserve < cfg.Reserve.Hours()*f.Hour {
			status.Reason, status.Speed = RunwayReserve, 0
		}
		if i == 0 || status.Speed < out.Speed {
			out = status
		}
	}
	return out
}

// observePoolLocked collects the accounts that serve model for provider, cooling-down
// ones included because they come back at their reset, with their windows and sizes.
// The caller holds m.mu.
func (m *Manager) observePoolLocked(ctx context.Context, pace *PaceSelector, provider, model string, now time.Time) []observedAccount {
	candidates, notCandidates := m.rankingCandidatesLocked(provider, model, now)
	pool := candidates
	for _, nc := range notCandidates {
		if nc.Reason == "cooldown" {
			pool = append(pool, nc.Auth)
		}
	}
	ctx = withSelectionModels(ctx, m.selectionModelsLocked(m.selector, pool, model))
	var out []observedAccount
	for i, auth := range pool {
		routing, tracked := quota.RoutingWindowsFor(auth.Provider, canonicalModelKey(selectionModelFromContext(ctx, auth, model)))
		if !tracked {
			continue
		}
		snapshot, ok := pace.view.Lookup(auth.ID, auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt)
		if !ok {
			continue
		}
		short, hasShort := snapshot.Window(routing.Short)
		if !hasShort {
			continue
		}
		obs := observedAccount{
			short:   routing.Short,
			windows: map[quota.WindowID]quota.Window{routing.Short: short},
			sizes:   map[quota.WindowID]float64{},
			usable:  i < len(candidates),
		}
		complete := true
		for j, slot := range routing.Long {
			w, found := snapshot.First(slot...)
			if !found {
				complete = false
				break
			}
			obs.windows[w.ID] = w
			group := planGroup{demand: w.ID, windows: []quota.WindowID{w.ID}}
			if j == 0 {
				group.windows = []quota.WindowID{routing.Short, w.ID}
			}
			obs.groups = append(obs.groups, group)
		}
		if !complete {
			continue
		}
		index := auth.EnsureIndex()
		for id, w := range obs.windows {
			if units, sized := m.ledger.Capacity(index, w, now); sized {
				obs.sizes[id] = units
			}
		}
		out = append(out, obs)
	}
	return out
}

// sizePool turns observed accounts into runway accounts. An account without a size, or
// sized far above its pool (most likely a reset was spent while the gateway was not
// watching, and oversizing would hide a dry pool), counts as the pool's median. ok is
// false when no account has sized a long window; a short window nobody sized is left out.
func sizePool(accounts []observedAccount, now time.Time) (sizedPool, bool) {
	estimates := map[quota.WindowID][]float64{}
	for _, obs := range accounts {
		for id, units := range obs.sizes {
			estimates[id] = append(estimates[id], units)
		}
	}
	medians := map[quota.WindowID]float64{}
	for id, values := range estimates {
		medians[id] = medianOf(values)
	}
	out := sizedPool{plans: map[quota.WindowID][]RunwayAccount{}}
	for _, obs := range accounts {
		room := math.Inf(1)
		for _, group := range obs.groups {
			var windows []RunwayWindow
			for _, id := range group.windows {
				median, known := medians[id]
				if !known {
					if id != obs.short {
						return sizedPool{}, false
					}
					// Planning on the long windows alone beats not throttling at all.
					if !slices.Contains(out.unsized, id) {
						out.unsized = append(out.unsized, id)
					}
					continue
				}
				units, sized := obs.sizes[id]
				if !sized || units > maxSizeOverMedian*median {
					units = median
				}
				w := runwayWindowOf(obs.windows[id], units, now)
				windows = append(windows, w)
				room = min(room, w.Remaining)
			}
			if _, seen := out.plans[group.demand]; !seen {
				out.order = append(out.order, group)
			}
			out.plans[group.demand] = append(out.plans[group.demand], RunwayAccount{Windows: windows})
		}
		if obs.usable && room > 0 && !math.IsInf(room, 1) {
			out.reserve += room
		}
	}
	return out, true
}

// runwayWindowOf sizes an observed window; a reset that has passed counts as refilled.
func runwayWindowOf(w quota.Window, capacity float64, now time.Time) RunwayWindow {
	out := RunwayWindow{ID: w.ID, Remaining: w.Remaining * capacity, Capacity: capacity, ResetAt: w.ResetAt, Length: w.Length}
	if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
		out.Remaining, out.ResetAt = capacity, time.Time{}
	}
	return out
}

// throttle delays a client request as long as the runway of the pools that can serve
// model asks for (RunwayStatus). It returns ctx's error when the client goes away
// during the wait.
func (m *Manager) throttle(ctx context.Context, providers []string, model string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	m.mu.RLock()
	status, ok := m.runwayLocked(ctx, providers, model, now)
	m.mu.RUnlock()
	if !ok || !status.Config.Enabled() {
		return nil
	}
	speed := 1.0
	if cycle, known := m.ledger.Cycle(now); known && status.Delay > 0 {
		speed = float64(cycle) / float64(cycle+status.Delay)
	}
	for _, provider := range providers {
		m.ledger.SetSpeed(provider, speed)
	}
	if status.Delay <= 0 {
		return nil
	}
	// The log formatter prints only the fields it knows, so the numbers go in the message.
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"provider": strings.Join(providers, ","),
		"model":    model,
		"reason":   string(status.Reason),
	}).Info(throttleLogMessage(status, now))
	wait := m.throttleWait
	if wait == nil {
		wait = waitContext
	}
	return wait(ctx, status.Delay)
}

// throttleLogMessage describes a delayed request's runway.
func throttleLogMessage(status RunwayStatus, now time.Time) string {
	msg := fmt.Sprintf("pace throttle: delaying request %s so the pool does not run dry (speed %.2f on %s, %d accounts, demand %.1f units/h",
		status.Delay.Round(time.Second), status.Speed, status.Window, status.Accounts, status.Demand)
	if !status.DryAt.IsZero() {
		msg += fmt.Sprintf(", dry in %s at full speed", status.DryAt.Sub(now).Round(time.Minute))
	}
	return msg + ")"
}

// waitContext waits d or until ctx ends, returning ctx's error then.
func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
