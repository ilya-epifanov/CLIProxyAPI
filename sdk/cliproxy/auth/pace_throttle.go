package auth

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

// ThrottleConfig slows every request down while the pool spends quota ahead of
// schedule, so that a busy pool runs out of a window at its reset instead of before it.
// A pool that runs out stops every client at once with 429s; one account that runs
// out only moves its sessions to another account.
type ThrottleConfig struct {
	// Threshold is the pool pace below which requests are delayed.
	Threshold float64
	// Floor is the pool pace at and below which requests wait MaxDelay.
	Floor float64
	// MaxDelay is the longest wait added to one request; 0 turns throttling off.
	MaxDelay time.Duration
}

// Enabled reports whether requests are ever delayed.
func (c ThrottleConfig) Enabled() bool {
	return c.MaxDelay > 0
}

// Delay returns MaxDelay × (Threshold − pace) / (Threshold − Floor), clamped to
// [0, MaxDelay]. A NaN pace (no quota data) is no evidence and gets no delay.
func (c ThrottleConfig) Delay(pace float64) time.Duration {
	if !c.Enabled() || math.IsNaN(pace) {
		return 0
	}
	f := (c.Threshold - pace) / (c.Threshold - c.Floor)
	return time.Duration(min(max(f, 0), 1) * float64(c.MaxDelay))
}

// PoolPace is the pace of a pool of accounts: for each window, the remaining fractions
// summed over the accounts divided by the time-left fractions summed over them. Pace is
// the lowest of those, set by Window.
type PoolPace struct {
	// Pace is NaN when no account has quota data.
	Pace   float64
	Window quota.WindowID
	// Accounts is how many accounts the pace covers.
	Accounts int
}

// PoolPace returns the pace of the accounts in auths that can serve now: every routing
// window of their selection model observed and none used up. An account that is used
// up or has no data says nothing about how fast the rest of the pool is burning. ctx
// carries the selection models (withSelectionModels) as it does for Pick.
func (s *PaceSelector) PoolPace(ctx context.Context, auths []*Auth, model string, now time.Time) PoolPace {
	type share struct{ remaining, timeLeft float64 }
	shares := map[quota.WindowID]*share{}
	var order []quota.WindowID
	out := PoolPace{Pace: math.NaN()}
	for _, auth := range auths {
		windows, ok := s.windowsOf(auth, selectionModelFromContext(ctx, auth, model))
		if !ok || !windows.complete {
			continue
		}
		all := windows.all()
		usable := true
		for _, w := range all {
			if exhausted(w, now) {
				usable = false
				break
			}
		}
		if !usable {
			continue
		}
		out.Accounts++
		for _, w := range all {
			remaining, timeLeft := windowShare(w, now)
			sum := shares[w.ID]
			if sum == nil {
				sum = &share{}
				shares[w.ID] = sum
				order = append(order, w.ID)
			}
			sum.remaining += remaining
			sum.timeLeft += timeLeft
		}
	}
	for _, id := range order {
		if p := shares[id].remaining / shares[id].timeLeft; math.IsNaN(out.Pace) || p < out.Pace {
			out.Pace, out.Window = p, id
		}
	}
	return out
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

// poolPaceLocked returns the pool pace of the candidates for model across providers and
// the pace selector, or ok false when the strategy is not pace. The caller holds m.mu.
func (m *Manager) poolPaceLocked(ctx context.Context, providers []string, model string, now time.Time) (PoolPace, *PaceSelector, bool) {
	pace, ok := paceSelectorOf(m.selector)
	if !ok {
		return PoolPace{}, nil, false
	}
	var candidates []*Auth
	for _, provider := range providers {
		providerCandidates, _ := m.rankingCandidatesLocked(provider, model, now)
		candidates = append(candidates, providerCandidates...)
	}
	ctx = withSelectionModels(ctx, m.selectionModelsLocked(m.selector, candidates, model))
	return pace.PoolPace(ctx, candidates, model, now), pace, true
}

// throttle delays a client request while the pool of accounts that can serve model
// spends quota ahead of schedule (ThrottleConfig). It returns ctx's error when the
// client goes away during the wait.
func (m *Manager) throttle(ctx context.Context, providers []string, model string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	m.mu.RLock()
	pool, pace, ok := m.poolPaceLocked(ctx, providers, model, now)
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	delay := pace.Throttle().Delay(pool.Pace)
	if delay <= 0 {
		return nil
	}
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"delay":     delay.Round(time.Second).String(),
		"pool_pace": math.Round(pool.Pace*100) / 100,
		"window":    pool.Window,
		"accounts":  pool.Accounts,
		"model":     model,
		"providers": strings.Join(providers, ","),
	}).Info("pace throttle: delaying request while the pool spends quota ahead of schedule")
	wait := m.throttleWait
	if wait == nil {
		wait = waitContext
	}
	return wait(ctx, delay)
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
