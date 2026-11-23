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
	// Reserve is the share of one window (the pool's median size) that the pool keeps
	// unspent at all times, in the plan and in the guard, so the last account does not
	// run to its very end before a refill.
	Reserve float64
	// GuardHorizon is how far ahead the guard looks: what the usable accounts hold now
	// must last, at the burn the quota reads show, until its window refills or for this
	// long. 0 turns the guard off.
	GuardHorizon time.Duration
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
	// RunwayReserve: at the burn the quota reads show, what the usable accounts hold runs
	// out before it refills (or within GuardHorizon), which slows requests more than the
	// plan does.
	RunwayReserve RunwayReason = "reserve"
	// RunwayLearning: no account has spent enough of any window to size it yet.
	RunwayLearning RunwayReason = "learning"
)

// RunwayStatus is a pool's runway and the throttle it sets.
type RunwayStatus struct {
	Reason RunwayReason
	// Speed is the share of the forecast the pool can serve for a week, or of the burn
	// under the guard; 1 when clear, NaN while learning.
	Speed float64
	// Delay is what a request waits now; 0 when throttling is off.
	Delay time.Duration
	// ComputedDelay is what a request would wait without Override.
	ComputedDelay time.Duration
	// delaySpeed is the speed a request runs at with Delay added to each turn; set with
	// Delay.
	delaySpeed float64
	// Override replaces Speed for the delay when the operator set one; Reason, Speed and
	// the rest stay what the plan and the guard computed.
	Override *ThrottleOverride
	// DryAt is when the pool runs dry at full speed; zero when it lasts a week.
	DryAt time.Time
	// Window is the long window of the plan that limits the speed.
	Window quota.WindowID
	// Accounts is how many accounts the plan spends.
	Accounts int
	// Demand is the next hour's forecast in cost units per hour, margin included.
	Demand float64
	// Unsized lists the windows no account has spent enough of to size yet; the plan
	// leaves them out, so it does not see their limit.
	Unsized []quota.WindowID
	Config  ThrottleConfig
}

// behindAffinity returns the selector session affinity wraps, or selector itself.
func behindAffinity(selector Selector) Selector {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		return affinity.fallback
	}
	return selector
}

// paceSelectorOf returns the PaceSelector behind selector, looking through session affinity.
func paceSelectorOf(selector Selector) (*PaceSelector, bool) {
	pace, ok := behindAffinity(selector).(*PaceSelector)
	return pace, ok && pace != nil
}

// ThrottleOverrides returns the per-pool speeds that replace the computed throttle.
func (m *Manager) ThrottleOverrides() *ThrottleOverrides {
	return m.throttleOverrides
}

// SetThrottleOverrides installs stored overrides; the service calls it before it serves requests.
func (m *Manager) SetThrottleOverrides(overrides *ThrottleOverrides) {
	m.throttleOverrides = overrides
}

// Ledger returns the usage ledger the throttle plans with. The service registers it as
// a usage plugin and seeds it from the usage store.
func (m *Manager) Ledger() *UsageLedger {
	return m.ledger
}

// observeRunwayLocked observes the pool behind each of providers for model, or returns ok
// false when the strategy is not pace. The caller holds m.mu; planRunway plans the pools
// without it.
func (m *Manager) observeRunwayLocked(providers []string, model string, now time.Time) (*PaceSelector, [][]observedAccount, bool) {
	pace, ok := paceSelectorOf(m.selector)
	if !ok || m.ledger == nil {
		return nil, nil, false
	}
	pools := make([][]observedAccount, len(providers))
	for i, provider := range providers {
		pools[i] = m.observePoolLocked(pace, provider, model, now)
	}
	return pace, pools, true
}

// planRunway plans the runway of the pools observeRunwayLocked observed behind
// providers, the tightest provider deciding. drain (drainServesLocked) runs requests at
// full speed: the operator wants the flagged accounts spent before their reset. It needs
// no manager lock.
func (m *Manager) planRunway(pace *PaceSelector, providers []string, pools [][]observedAccount, drain bool, now time.Time) RunwayStatus {
	cfg := pace.cfg.Throttle
	var out RunwayStatus
	for i, provider := range providers {
		status := m.providerRunway(cfg, provider, pools[i], now)
		if o, set := m.throttleOverrides.Get(provider); set {
			status.Override = &o
		}
		if i == 0 || runwayTighter(status, out) {
			out = status
		}
	}
	out.Config = cfg
	if cfg.Enabled() {
		out.ComputedDelay, out.delaySpeed = m.throttleDelay(cfg, out.Speed, out.Reason != RunwayLearning, now)
		out.Delay = out.ComputedDelay
		if o := out.Override; o != nil {
			out.Delay, out.delaySpeed = m.throttleDelay(cfg, o.Speed, true, now)
		}
		if drain {
			out.Delay, out.delaySpeed = 0, 1
		}
	}
	return out
}

// runSpeed returns the speed requests run at: the override when one is set, else the
// computed speed; ok is false while learning, when nothing is delayed.
func (s RunwayStatus) runSpeed() (speed float64, ok bool) {
	if s.Override != nil {
		return s.Override.Speed, true
	}
	if s.Reason == RunwayLearning {
		return math.NaN(), false
	}
	return s.Speed, true
}

// runwayTighter reports whether a throttles harder than b; learning throttles least.
func runwayTighter(a, b RunwayStatus) bool {
	speedA, okA := a.runSpeed()
	speedB, okB := b.runSpeed()
	if !okA || !okB {
		return okA && !okB
	}
	return speedA < speedB
}

// throttleDelay turns a speed into a wait: an agent loop of turns cycle long runs at
// cycle / (cycle + delay), which it returns as the speed the wait gives (1 without a
// turn length). Without a turn length it waits MaxDelay. A pool still learning (sized
// false) is not delayed.
func (m *Manager) throttleDelay(cfg ThrottleConfig, speed float64, sized bool, now time.Time) (time.Duration, float64) {
	if !sized || speed >= 1 {
		return 0, 1
	}
	cycle, ok := m.ledger.Cycle(now)
	if !ok {
		return cfg.MaxDelay, 1
	}
	delay := cfg.MaxDelay
	if speed > 0 {
		delay = min(time.Duration(float64(cycle)*(1/speed-1)), cfg.MaxDelay)
	}
	if delay <= 0 {
		return delay, 1
	}
	return delay, float64(cycle) / float64(cycle+delay)
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
	windows map[quota.WindowID]quota.Window
	sizes   map[quota.WindowID]float64
	// burns is the share of each window spent per hour, from the quota reads.
	burns  map[quota.WindowID]float64
	groups []planGroup
	// usable: a candidate now, not cooling down.
	usable bool
}

// sizedPool is a pool ready to plan: per demand window, the accounts that spend it.
type sizedPool struct {
	order []planGroup
	plans map[quota.WindowID][]RunwayAccount
	guard map[quota.WindowID]*guardWindow
	// medians is each window's median size across the pool, in units.
	medians map[quota.WindowID]float64
	unsized []quota.WindowID
}

// keep is what the pool holds back of each window: share of its median size.
func (p sizedPool) keep(share float64) map[quota.WindowID]float64 {
	out := make(map[quota.WindowID]float64, len(p.medians))
	for id, median := range p.medians {
		out[id] = share * median
	}
	return out
}

// guardWindow is one window across the pool, in units of the pool's median size for it,
// so an error in every size alike cancels out.
type guardWindow struct {
	// room is what the usable accounts hold now; burn what the pool spent per hour.
	room, burn float64
	// refill is the soonest pending reset; zero when none.
	refill time.Time
}

// providerRunway plans one provider's observed pool. The short window and the first long
// slot share all of the provider's demand, so they are planned together; a further long
// slot (the Fable window) is spent only by its own models and is planned alone.
func (m *Manager) providerRunway(cfg ThrottleConfig, provider string, observed []observedAccount, now time.Time) RunwayStatus {
	pool := sizePool(observed, now)
	if len(pool.order) == 0 {
		return RunwayStatus{Reason: RunwayLearning, Speed: math.NaN()}
	}
	margin := 1 + cfg.Margin
	keep := pool.keep(cfg.Reserve)
	var out RunwayStatus
	for i, group := range pool.order {
		f := m.ledger.Forecast(provider, group.demand, now)
		f = Forecast{Hour: f.Hour * margin, Day: f.Day * margin, Week: f.Week * margin}
		accounts := pool.plans[group.demand]
		plan := PlanRunway(accounts, f, keep, now)
		status := RunwayStatus{Reason: RunwayClear, Speed: plan.Speed, DryAt: plan.DryAt, Window: group.demand, Accounts: len(accounts), Demand: f.Hour, Unsized: pool.unsized}
		if plan.Speed < 1 {
			status.Reason = RunwayShort
		}
		if i == 0 {
			speed, under := pool.guardSpeed(cfg.GuardHorizon, cfg.Margin, cfg.Reserve, m.ledger.MeanSpeed(provider, now), now)
			if under && speed < status.Speed {
				status.Reason, status.Speed = RunwayReserve, speed
			}
		}
		if i == 0 || status.Speed < out.Speed {
			out = status
		}
	}
	return out
}

// observePoolLocked collects the accounts that serve model for provider, cooling-down
// ones included because they come back at their reset, with their windows and sizes.
// An account needs every long window observed; the short one it may lack. The caller
// holds m.mu.
func (m *Manager) observePoolLocked(pace *PaceSelector, provider, model string, now time.Time) []observedAccount {
	accounts := m.rankingPoolLocked(provider, model, now)
	// Candidates first, then the accounts cooling down.
	pool := make([]rankingAccount, 0, len(accounts))
	for _, account := range accounts {
		if !account.blocked {
			pool = append(pool, account)
		}
	}
	usable := len(pool)
	for _, account := range accounts {
		if account.blocked && account.reason == blockReasonCooldown {
			pool = append(pool, account)
		}
	}
	var out []observedAccount
	for i, account := range pool {
		windows, ok := pace.windowsOf(account.auth, account.model)
		if !ok || !windows.Complete {
			continue
		}
		obs := observedAccount{
			windows: map[quota.WindowID]quota.Window{},
			sizes:   map[quota.WindowID]float64{},
			burns:   map[quota.WindowID]float64{},
			usable:  i < usable,
		}
		// An account without the short window is not capped by it, as in the selector.
		if windows.HasShort {
			obs.windows[windows.Short.ID] = windows.Short
		}
		for j, w := range windows.Long {
			obs.windows[w.ID] = w
			group := planGroup{demand: w.ID, windows: []quota.WindowID{w.ID}}
			if j == 0 && windows.HasShort {
				group.windows = []quota.WindowID{windows.Short.ID, w.ID}
			}
			obs.groups = append(obs.groups, group)
		}
		// Every stored account has its index; EnsureIndex would write it under the read lock.
		index := account.auth.Index
		for id, w := range obs.windows {
			m.ledger.Observe(index, w)
			if units, sized := m.ledger.Capacity(index, w, now); sized {
				obs.sizes[id] = units
			}
			if burn, known := m.ledger.Burn(index, id, now); known {
				obs.burns[id] = burn
			}
		}
		out = append(out, obs)
	}
	return out
}

// sizePool turns observed accounts into runway accounts. An account without a size, or
// sized far above its pool (most likely a reset was spent while the gateway was not
// watching, and oversizing would hide a dry pool), counts as the pool's median. A window
// nobody has sized is left out; a plan left with no window is dropped. A window some
// accounts lack is not guarded: they keep serving when it runs out.
func sizePool(accounts []observedAccount, now time.Time) sizedPool {
	estimates := map[quota.WindowID][]float64{}
	holders := map[quota.WindowID]int{}
	for _, obs := range accounts {
		for id, units := range obs.sizes {
			estimates[id] = append(estimates[id], units)
		}
		for id := range obs.windows {
			holders[id]++
		}
	}
	medians := map[quota.WindowID]float64{}
	for id, values := range estimates {
		medians[id] = medianOf(values)
	}
	out := sizedPool{plans: map[quota.WindowID][]RunwayAccount{}, guard: map[quota.WindowID]*guardWindow{}, medians: medians}
	for _, obs := range accounts {
		guarded := map[quota.WindowID]bool{}
		for _, group := range obs.groups {
			var windows []RunwayWindow
			for _, id := range group.windows {
				median, known := medians[id]
				if !known {
					// Planning on the sized windows alone beats not throttling at all.
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
				if !guarded[id] {
					guarded[id] = true
					out.guard[id] = out.guard[id].add(w, units/median, obs.burns[id], obs.usable)
				}
			}
			if len(windows) == 0 {
				continue
			}
			if _, seen := out.plans[group.demand]; !seen {
				out.order = append(out.order, group)
			}
			out.plans[group.demand] = append(out.plans[group.demand], RunwayAccount{Windows: windows})
		}
	}
	for id := range out.guard {
		if holders[id] < len(accounts) {
			delete(out.guard, id)
		}
	}
	return out
}

// add counts one account's window w, sized relative to the pool's median.
func (g *guardWindow) add(w RunwayWindow, rel, burn float64, usable bool) *guardWindow {
	if g == nil {
		g = &guardWindow{}
	}
	if usable {
		g.room += w.Remaining / w.Capacity * rel
	}
	g.burn += burn * rel
	if !w.ResetAt.IsZero() && (g.refill.IsZero() || w.ResetAt.Before(g.refill)) {
		g.refill = w.ResetAt
	}
	return g
}

// guardSpeed is the guard (ThrottleConfig.GuardHorizon). It needs no window size: for
// each window, the share the usable accounts hold, less reserve of a median window, over
// the share the pool burns per hour (undone by the speed it ran at, inflated by margin)
// is how long they last. When that is shorter than the time to the window's refill,
// capped at horizon, under is true and speed is the share of the burn that lasts until
// then; the lowest over the windows wins.
func (p sizedPool) guardSpeed(horizon time.Duration, margin, reserve, speedNow float64, now time.Time) (float64, bool) {
	if horizon <= 0 {
		return 0, false
	}
	speed, under := 1.0, false
	for _, g := range p.guard {
		if g.burn <= 0 {
			continue
		}
		burn := g.burn / max(speedNow, 1e-3) * (1 + margin)
		lasts := max(g.room-reserve, 0) / burn
		until := horizon
		if !g.refill.IsZero() {
			until = min(until, max(g.refill.Sub(now), time.Minute))
		}
		if lasts < until.Hours() {
			speed, under = min(speed, lasts/until.Hours()), true
		}
	}
	return speed, under
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
	pace, pools, ok := m.observeRunwayLocked(providers, model, now)
	drain := ok && m.drainServesLocked(pace, providers, model, now)
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	status := m.planRunway(pace, providers, pools, drain, now)
	if !status.Config.Enabled() {
		return nil
	}
	for _, provider := range providers {
		m.ledger.SetSpeed(provider, status.delaySpeed, now)
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
		wait = realContinuityClock{}.Wait
	}
	return wait(ctx, status.Delay)
}

// throttleLogMessage describes a delayed request's runway.
func throttleLogMessage(status RunwayStatus, now time.Time) string {
	if o := status.Override; o != nil {
		return fmt.Sprintf("pace throttle: delaying request %s at the override speed %.2f (computed: %s, wait %s)",
			status.Delay.Round(time.Second), o.Speed, status.Reason, status.ComputedDelay.Round(time.Second))
	}
	msg := fmt.Sprintf("pace throttle: delaying request %s so the pool does not run dry (speed %.2f on %s, %d accounts, demand %.1f units/h",
		status.Delay.Round(time.Second), status.Speed, status.Window, status.Accounts, status.Demand)
	if !status.DryAt.IsZero() {
		msg += fmt.Sprintf(", dry in %s at full speed", status.DryAt.Sub(now).Round(time.Minute))
	}
	return msg + ")"
}
