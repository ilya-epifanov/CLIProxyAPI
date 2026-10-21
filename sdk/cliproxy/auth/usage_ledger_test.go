package auth

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func ledgerEntry(ago time.Duration, authIndex, model string, units float64) LedgerEntry {
	return LedgerEntry{At: paceNow.Add(-ago), Provider: "claude", AuthIndex: authIndex, Model: model, Units: units}
}

func TestMinuteSeriesSumsPartialAndWholeHours(t *testing.T) {
	s := newMinuteSeries()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for _, m := range []int{5, 59, 60, 61, 125, 179} {
		s.add(base.Add(time.Duration(m)*time.Minute), 1)
	}
	for _, tc := range []struct {
		from, to int
		want     float64
	}{
		{0, 180, 6},
		{5, 6, 1},
		{6, 60, 1},
		{59, 126, 4},
		{60, 120, 2},
		{126, 180, 1},
	} {
		if got := s.sum(base.Add(time.Duration(tc.from)*time.Minute), base.Add(time.Duration(tc.to)*time.Minute)); got != tc.want {
			t.Errorf("sum(%d, %d) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
	// A week later the slots are reused; the old values are gone.
	later := base.Add(ledgerSpan + 2*time.Hour)
	if got := s.sum(later.Add(-ledgerSpan), later); got != 2 {
		t.Errorf("sum over the last week = %v, want 2 (minutes 125 and 179)", got)
	}
}

func TestLedgerCountsTheWindowsEachModelSpends(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l.Add(ledgerEntry(30*time.Minute, "a", "claude-opus-5-5", 2), false)
	l.Add(ledgerEntry(20*time.Minute, "a", "claude-fable-5-1", 3), false)

	f := l.Forecast("claude", quota.SevenDay, paceNow)
	if f.Hour != 5 {
		t.Fatalf("seven-day demand in the last hour = %v, want 5", f.Hour)
	}
	if f := l.Forecast("claude", quota.SevenDayFable, paceNow); f.Hour != 3 {
		t.Fatalf("Fable demand in the last hour = %v, want 3", f.Hour)
	}
	if f := l.Forecast("claude", quota.FiveHour, paceNow); f.Hour != 5 {
		t.Fatalf("five-hour demand in the last hour = %v, want 5", f.Hour)
	}
	if math.Abs(f.Day-5.0/24) > 1e-9 || math.Abs(f.Week-5.0/168) > 1e-9 {
		t.Fatalf("forecast = %+v; want the day and week averages", f)
	}
}

func TestLedgerAveragesOverTheSpanItCovers(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-2 * time.Hour))
	l.Add(ledgerEntry(90*time.Minute, "a", "claude-opus-5-5", 4), true)
	f := l.Forecast("claude", quota.SevenDay, paceNow)
	if f.Day != 2 || f.Week != 2 {
		t.Fatalf("forecast = %+v; want 4 units over the 2 h covered", f)
	}
}

func TestLedgerScalesLiveDemandByTheThrottleSpeed(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l.SetSpeed("claude", 0.5)
	l.Add(ledgerEntry(10*time.Minute, "a", "claude-opus-5-5", 1), true)
	l.Add(ledgerEntry(10*time.Minute, "a", "claude-opus-5-5", 1), false)
	if f := l.Forecast("claude", quota.SevenDay, paceNow); f.Hour != 3 {
		t.Fatalf("demand = %v, want 2 (live, at half speed) + 1 (seeded)", f.Hour)
	}
	// Spend, which sizes the windows, is never scaled.
	w := quota.Window{ID: quota.SevenDay, Remaining: 0.8, ResetAt: paceNow.Add(week / 2), Length: week, ObservedAt: paceNow}
	if units, ok := l.Capacity("a", w, paceNow); !ok || math.Abs(units-10) > 1e-9 {
		t.Fatalf("capacity = %v, %v; want 2 units / 0.2 = 10", units, ok)
	}
}

func TestLedgerCapacityFromTheWindowStart(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l.Add(ledgerEntry(4*24*time.Hour, "a", "claude-opus-5-5", 50), false) // before the window
	l.Add(ledgerEntry(2*24*time.Hour, "a", "claude-opus-5-5", 120), false)
	l.Add(ledgerEntry(time.Hour, "a", "claude-opus-5-5", 40), false)
	w := quota.Window{ID: quota.SevenDay, Remaining: 0.8, ResetAt: paceNow.Add(4 * 24 * time.Hour), Length: week, ObservedAt: paceNow}

	units, ok := l.Capacity("a", w, paceNow)
	if !ok || math.Abs(units-800) > 1e-9 {
		t.Fatalf("capacity = %v, %v; want 160 units / 0.2 = 800", units, ok)
	}
	if _, ok := l.Capacity("b", w, paceNow); ok {
		t.Fatal("an account with no recorded spend has no capacity")
	}
	idle := w
	idle.ResetAt = time.Time{}
	if _, ok := NewUsageLedger(paceNow.Add(-ledgerSpan)).Capacity("a", idle, paceNow); ok {
		t.Fatal("an idle window with no earlier estimate has none")
	}
	thin := w
	thin.Remaining = 0.95
	l2 := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l2.Add(ledgerEntry(time.Hour, "a", "claude-opus-5-5", 40), false)
	if _, ok := l2.Capacity("a", thin, paceNow); ok {
		t.Fatal("5 % spent is too little evidence")
	}
}

func TestLedgerCapacityFromTheFirstObservationWhenTheStartIsNotCovered(t *testing.T) {
	start := paceNow.Add(-3 * time.Hour)
	l := NewUsageLedger(start)
	first := quota.Window{ID: quota.SevenDay, Remaining: 0.5, ResetAt: paceNow.Add(4 * 24 * time.Hour), Length: week, ObservedAt: start}
	if _, ok := l.Capacity("a", first, start); ok {
		t.Fatal("the first observation only anchors the estimate")
	}
	l.Add(ledgerEntry(2*time.Hour, "a", "claude-opus-5-5", 80), false)
	later := first
	later.Remaining, later.ObservedAt = 0.4, paceNow
	if units, ok := l.Capacity("a", later, paceNow); !ok || math.Abs(units-800) > 1e-6 {
		t.Fatalf("capacity = %v, %v; want 80 units / 0.1 = 800", units, ok)
	}
	// A new window (another reset) starts without evidence; the last estimate stands.
	next := later
	next.ResetAt, next.Remaining = paceNow.Add(week), 1
	if units, ok := l.Capacity("a", next, paceNow); !ok || math.Abs(units-800) > 1e-6 {
		t.Fatalf("after a reset: capacity = %v, %v; want the last estimate, 800", units, ok)
	}
}

func TestLedgerCycleAveragesSessionGaps(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	if _, ok := l.Cycle(paceNow); ok {
		t.Fatal("no sessions: no cycle")
	}
	for _, e := range []struct {
		ago     time.Duration
		session string
	}{
		{20 * time.Minute, "s1"}, {19*time.Minute + 50*time.Second, "s1"}, {19*time.Minute + 20*time.Second, "s1"},
		{5 * time.Minute, "s2"}, {4 * time.Minute, "s2"},
		{50 * time.Minute, "s3"}, {25 * time.Minute, "s3"}, // a 25-minute gap is a person, not a turn
	} {
		l.Add(LedgerEntry{At: paceNow.Add(-e.ago), Provider: "claude", Model: "claude-opus-5-5", SessionID: e.session}, true)
	}
	cycle, ok := l.Cycle(paceNow)
	if !ok || cycle != (10+30+60)*time.Second/3 {
		t.Fatalf("cycle = %s, %v; want the mean of 10 s, 30 s, 60 s", cycle, ok)
	}
}

func TestLedgerHandleUsageCountsCostUnits(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-opus-5-5",
		AuthIndex:   "a",
		RequestedAt: paceNow.Add(-time.Minute),
		Detail: coreusage.Detail{TokenBreakdown: coreusage.NewIndependentTokenBreakdown(
			1_000_000, 10_000_000, 0, 200_000, 0, 11_200_000)},
	})
	if f := l.Forecast("claude", quota.SevenDay, paceNow); math.Abs(f.Hour-3) > 1e-9 {
		t.Fatalf("demand = %v, want 1 + 1 + 1 units", f.Hour)
	}
}

func TestLedgerCapacityStartsOverAfterASpentReset(t *testing.T) {
	l := NewUsageLedger(paceNow.Add(-ledgerSpan))
	l.Add(ledgerEntry(3*time.Hour, "a", "claude-opus-5-5", 400), false)
	reset := paceNow.Add(4 * 24 * time.Hour)
	before := quota.Window{ID: quota.SevenDay, Remaining: 0.5, ResetAt: reset, Length: week, ObservedAt: paceNow.Add(-2 * time.Hour)}
	if units, ok := l.Capacity("a", before, paceNow.Add(-2*time.Hour)); !ok || math.Abs(units-800) > 1e-6 {
		t.Fatalf("before the reset: %v, %v; want 800", units, ok)
	}
	// The reset refills the window; 20 % is spent on 160 units after it.
	l.Add(ledgerEntry(90*time.Minute, "a", "claude-opus-5-5", 160), false)
	after := quota.Window{ID: quota.SevenDay, Remaining: 1, ResetAt: reset, Length: week, ObservedAt: paceNow.Add(-100 * time.Minute)}
	l.Capacity("a", after, paceNow.Add(-100*time.Minute))
	now := after
	now.Remaining, now.ObservedAt = 0.8, paceNow
	if units, ok := l.Capacity("a", now, paceNow); !ok || math.Abs(units-800) > 1e-6 {
		t.Fatalf("after the reset: %v, %v; want 160 / 0.2 = 800, not (400 + 160) / 0.2", units, ok)
	}
}
