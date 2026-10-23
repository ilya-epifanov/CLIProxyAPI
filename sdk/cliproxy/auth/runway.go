package auth

import (
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// CostUnits weights a request's tokens by API price ratios, so quota spent on cache reads
// and on output compares: 1 unit is 1M uncached input tokens, a cache read 0.1, a cache
// write 1.25, output and reasoning 5. Unclassified tokens count as input.
func CostUnits(b coreusage.TokenBreakdown) float64 {
	return CostUnitsOf(b.Input.UncachedTokens, b.Input.CacheReadTokens, b.Input.CacheWriteTokens,
		b.Output.NonReasoningTokens, b.Output.ReasoningTokens, b.UnclassifiedTokens)
}

// CostUnitsOf is CostUnits over the disjoint token buckets.
func CostUnitsOf(input, cacheRead, cacheWrite, output, reasoning, unclassified int64) float64 {
	tokens := float64(input) + 0.1*float64(cacheRead) + 1.25*float64(cacheWrite) +
		5*float64(output+reasoning) + float64(unclassified)
	return tokens / 1e6
}

// RunwayWindow is one quota window of an account, sized in cost units.
type RunwayWindow struct {
	ID        quota.WindowID
	Remaining float64
	Capacity  float64
	// ResetAt is zero when no reset is pending: the window's clock starts on first use.
	ResetAt time.Time
	Length  time.Duration
}

// RunwayAccount is the windows one account spends on every request; all need room.
type RunwayAccount struct {
	Windows []RunwayWindow
}

// Forecast is the demand expected in cost units per hour: Hour for the next hour, Day
// up to a day ahead, Week beyond.
type Forecast struct {
	Hour, Day, Week float64
}

func (f Forecast) at(ahead time.Duration) float64 {
	switch {
	case ahead < time.Hour:
		return f.Hour
	case ahead < 24*time.Hour:
		return f.Day
	default:
		return f.Week
	}
}

// RunwayPlan is how fast the pool can run without running dry.
type RunwayPlan struct {
	// Speed is the highest share (0..1) of the forecast at which every step is served.
	Speed float64
	// DryAt is when the pool runs out at full speed, inside the first step it cannot
	// fully serve; zero when it lasts the horizon.
	DryAt time.Time
}

const (
	runwayHorizon = 7 * 24 * time.Hour
	// runwayFineSpan is planned in fine steps, where 5-hour windows decide.
	runwayFineSpan = 12 * time.Hour
	runwayFineStep = 15 * time.Minute
	runwayStep     = time.Hour
	runwaySearches = 10
)

// PlanRunway spends the forecast from the accounts over the next week, the account whose
// longest window resets first spending first, because quota left at a reset is lost.
// Every account is capped by each of its windows, a window refills at its reset, and an
// idle window starts its clock when it is first spent. The pool never spends the last
// keep[id] units of a window id, summed over its accounts.
func PlanRunway(accounts []RunwayAccount, f Forecast, keep map[quota.WindowID]float64, now time.Time) RunwayPlan {
	if dryAt, ok := runwayServes(accounts, f, keep, 1, now); !ok {
		lo, hi := 0.0, 1.0
		for range runwaySearches {
			mid := (lo + hi) / 2
			if _, served := runwayServes(accounts, f, keep, mid, now); served {
				lo = mid
			} else {
				hi = mid
			}
		}
		return RunwayPlan{Speed: lo, DryAt: dryAt}
	}
	return RunwayPlan{Speed: 1}
}

// runwayServes reports whether scale × the forecast is fully served over the horizon,
// and otherwise the first step that is not.
func runwayServes(accounts []RunwayAccount, f Forecast, keep map[quota.WindowID]float64, scale float64, now time.Time) (time.Time, bool) {
	state := make([][]RunwayWindow, len(accounts))
	for i, account := range accounts {
		state[i] = append([]RunwayWindow(nil), account.Windows...)
	}
	order := make([]int, len(state))
	for ahead := time.Duration(0); ahead < runwayHorizon; {
		step := runwayStep
		if ahead < runwayFineSpan {
			step = runwayFineStep
		}
		at := now.Add(ahead)
		for _, windows := range state {
			for j := range windows {
				if w := &windows[j]; !w.ResetAt.IsZero() && !at.Before(w.ResetAt) {
					w.Remaining, w.ResetAt = w.Capacity, time.Time{}
				}
			}
		}
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			return expiryOf(state[order[a]]).Before(expiryOf(state[order[b]]))
		})
		// What the pool may still spend of each window, above what it keeps.
		spendable := map[quota.WindowID]float64{}
		for _, windows := range state {
			for _, w := range windows {
				spendable[w.ID] += w.Remaining
			}
		}
		for id := range spendable {
			spendable[id] -= keep[id]
		}
		want := scale * f.at(ahead) * step.Hours()
		need := want
		for _, i := range order {
			if need <= 0 {
				break
			}
			take := need
			for _, w := range state[i] {
				take = min(take, w.Remaining, spendable[w.ID])
			}
			if take <= 0 {
				continue
			}
			for j := range state[i] {
				w := &state[i][j]
				if w.ResetAt.IsZero() {
					w.ResetAt = at.Add(w.Length)
				}
				w.Remaining -= take
				spendable[w.ID] -= take
			}
			need -= take
		}
		// A millionth of a unit is far below one request; it only absorbs rounding.
		if need > 1e-6 {
			// Demand is even over the step: it runs out after the share that was served.
			return at.Add(time.Duration((want - need) / want * float64(step))), false
		}
		ahead += step
	}
	return time.Time{}, true
}

// expiryOf is the reset of the account's longest window; an idle one sorts last.
func expiryOf(windows []RunwayWindow) time.Time {
	var longest RunwayWindow
	for _, w := range windows {
		if w.Length > longest.Length {
			longest = w
		}
	}
	if longest.ResetAt.IsZero() {
		return time.Unix(math.MaxInt32, 0)
	}
	return longest.ResetAt
}
