package auth

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func runwayWindow(id quota.WindowID, remaining, capacity float64, resetIn, length time.Duration) RunwayWindow {
	w := RunwayWindow{ID: id, Remaining: remaining, Capacity: capacity, Length: length}
	if resetIn != noReset {
		w.ResetAt = paceNow.Add(resetIn)
	}
	return w
}

func TestCostUnitsWeighsTokensByPrice(t *testing.T) {
	b := coreusage.TokenBreakdown{
		Input:              coreusage.TokenInputBreakdown{UncachedTokens: 1_000_000, CacheReadTokens: 10_000_000, CacheWriteTokens: 800_000},
		Output:             coreusage.TokenOutputBreakdown{NonReasoningTokens: 100_000, ReasoningTokens: 100_000},
		UnclassifiedTokens: 500_000,
	}
	// 1 + 1 + 1 + 1 + 0.5
	if got := CostUnits(b); math.Abs(got-4.5) > 1e-9 {
		t.Fatalf("CostUnits = %v, want 4.5", got)
	}
}

func TestPlanRunwayClearWhenSupplyLasts(t *testing.T) {
	accounts := []RunwayAccount{{Windows: []RunwayWindow{
		runwayWindow(quota.FiveHour, 200, 200, noReset, fiveHour),
		runwayWindow(quota.SevenDay, 800, 800, noReset, week),
	}}}
	plan := PlanRunway(accounts, Forecast{Hour: 2, Day: 2, Week: 2}, paceNow)
	if plan.Speed != 1 || !plan.DryAt.IsZero() {
		t.Fatalf("plan = %+v; want full speed, never dry", plan)
	}
}

func TestPlanRunwaySpendsTheSoonestResetFirst(t *testing.T) {
	// A's 100 units are lost at its reset in 10 h unless spent first; B's last the week.
	// 240 units in the first day fit only when A spends first: A 0-10 h, B 10-20 h, A again.
	accounts := []RunwayAccount{
		{Windows: []RunwayWindow{runwayWindow(quota.SevenDay, 100, 100, 200*time.Hour, week)}},
		{Windows: []RunwayWindow{runwayWindow(quota.SevenDay, 100, 100, 10*time.Hour, week)}},
	}
	plan := PlanRunway(accounts, Forecast{Hour: 10, Day: 10}, paceNow)
	if plan.Speed != 1 {
		t.Fatalf("plan = %+v; want full speed", plan)
	}
}

func TestPlanRunwayFindsTheSpeedThatLastsTheWeek(t *testing.T) {
	// 100 units, no refill in the horizon, 1 unit per hour wanted: 100 / 168 of the demand.
	accounts := []RunwayAccount{{Windows: []RunwayWindow{runwayWindow(quota.SevenDay, 100, 800, 200*time.Hour, week)}}}
	plan := PlanRunway(accounts, Forecast{Hour: 1, Day: 1, Week: 1}, paceNow)
	if math.Abs(plan.Speed-100.0/168) > 0.002 {
		t.Fatalf("speed = %v, want about %v", plan.Speed, 100.0/168)
	}
	if dry := plan.DryAt.Sub(paceNow); dry < 99*time.Hour || dry > 101*time.Hour {
		t.Fatalf("dry in %s, want about 100 h", dry)
	}
}

func TestPlanRunwayIsCappedByTheFiveHourWindow(t *testing.T) {
	// Plenty of weekly quota, but 20 units per 5 h is 4 per hour.
	short := runwayWindow(quota.FiveHour, 20, 20, noReset, fiveHour)
	long := runwayWindow(quota.SevenDay, 1000, 1000, noReset, week)
	accounts := []RunwayAccount{{Windows: []RunwayWindow{short, long}}}

	fast := PlanRunway(accounts, Forecast{Hour: 30, Day: 30, Week: 30}, paceNow)
	if fast.Speed >= 1 || fast.DryAt.Sub(paceNow) > time.Hour {
		t.Fatalf("30/h plan = %+v; want throttled, dry within the first hour", fast)
	}
	if slow := PlanRunway(accounts, Forecast{Hour: 3, Day: 3, Week: 3}, paceNow); slow.Speed != 1 {
		t.Fatalf("3/h plan = %+v; want full speed", slow)
	}
}

func TestPlanRunwayCountsRefills(t *testing.T) {
	// Empty now, full again in an hour: dry for the first hour only, and
	// the speed that lasts the week is 0 (nothing can be served before the reset).
	accounts := []RunwayAccount{{Windows: []RunwayWindow{runwayWindow(quota.SevenDay, 0, 800, time.Hour, week)}}}
	plan := PlanRunway(accounts, Forecast{Hour: 1, Day: 1, Week: 1}, paceNow)
	if plan.Speed > 0.001 || !plan.DryAt.Equal(paceNow) {
		t.Fatalf("plan = %+v; want speed 0, dry now", plan)
	}
	if plan := PlanRunway(accounts, Forecast{Day: 4, Week: 4}, paceNow); plan.Speed != 1 {
		t.Fatalf("no demand in the first hour: plan = %+v; want full speed", plan)
	}
}

func TestPlanRunwayDryAtFallsInsideTheStep(t *testing.T) {
	// 0.8 units at 3.6 an hour last about 13 min, inside the first 15-minute step.
	accounts := []RunwayAccount{{Windows: []RunwayWindow{
		runwayWindow(quota.FiveHour, 0.8, 5.4, 45*time.Minute, fiveHour),
		runwayWindow(quota.SevenDay, 40, 47, 160*time.Hour, week),
	}}}
	plan := PlanRunway(accounts, Forecast{Hour: 3.6, Day: 3.6, Week: 3.6}, paceNow)
	if dry := plan.DryAt.Sub(paceNow); (dry - 800*time.Second).Abs() > time.Second {
		t.Fatalf("dry in %s, want 0.8 / 3.6 h = 13m20s", dry)
	}
}
