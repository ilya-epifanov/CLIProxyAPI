package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	ledgerSpan = 7 * 24 * time.Hour
	// ledgerSessionGap is the longest gap between two requests of a session that still
	// counts as one agent turn following another, not a person coming back.
	ledgerSessionGap = 10 * time.Minute
	// ledgerCycleSpan is how far back the turn-to-turn gap is averaged.
	ledgerCycleSpan = 30 * time.Minute
	// burnSpan is how far back the guard reads a window's burn from the quota reads.
	burnSpan = time.Hour
	// minBurnSpan is the shortest span of reads a burn is taken from.
	minBurnSpan = 30 * time.Minute
	// minCapacityEvidence is the share of a window that must have been spent since the
	// capacity anchor before the units spent say how large the window is. Header
	// utilizations are whole percents.
	minCapacityEvidence = 0.1
)

// UsageLedger keeps a week of upstream usage in cost units (CostUnits): per account and
// quota window, to size each account's windows, and per provider and window, to forecast
// the pool's demand. Demand is recorded as it would have been without the throttle. It
// is a usage.Plugin. Safe for concurrent use.
type UsageLedger struct {
	mu sync.Mutex
	// since is the earliest instant the ledger has seen all usage from.
	since    time.Time
	accounts map[ledgerKey]*minuteSeries
	demand   map[ledgerKey]*minuteSeries
	// speeds is the share of its unthrottled rate each provider runs at now.
	speeds  map[string]float64
	last    map[string]time.Time // session → its latest request
	gaps    []ledgerGap
	anchors map[ledgerKey]capacityAnchor
	// sized is each account window's latest estimate, kept across its resets.
	sized map[ledgerKey]float64
	// observed is each account window's recent share used, as the quota reads show it.
	observed map[ledgerKey][]shareObservation
	// speedLog is each provider's recent speeds, one sample a minute at most.
	speedLog map[string][]speedSample
}

type shareObservation struct {
	at      time.Time
	used    float64
	resetAt time.Time
}

type speedSample struct {
	at    time.Time
	speed float64
}

var _ coreusage.Plugin = (*UsageLedger)(nil)

// ledgerKey is an account (auth index) or a provider, and one of its windows.
type ledgerKey struct {
	owner  string
	window quota.WindowID
}

type ledgerGap struct {
	at  time.Time
	gap time.Duration
}

// capacityAnchor is where the spend that sizes a window is counted from: the window's
// start when the ledger covers it, otherwise the first observation after the ledger did,
// and again after a spent reset.
type capacityAnchor struct {
	resetAt time.Time
	at      time.Time
	used    float64
	// lastUsed is the latest observed share used; a drop means a reset was spent.
	lastUsed float64
}

// NewUsageLedger returns an empty ledger that has seen usage since now.
func NewUsageLedger(now time.Time) *UsageLedger {
	return &UsageLedger{
		since:    now,
		accounts: map[ledgerKey]*minuteSeries{},
		demand:   map[ledgerKey]*minuteSeries{},
		speeds:   map[string]float64{},
		last:     map[string]time.Time{},
		anchors:  map[ledgerKey]capacityAnchor{},
		sized:    map[ledgerKey]float64{},
		observed: map[ledgerKey][]shareObservation{},
		speedLog: map[string][]speedSample{},
	}
}

// LedgerEntry is one upstream request as the ledger counts it.
type LedgerEntry struct {
	At        time.Time
	Provider  string
	AuthIndex string
	Model     string
	SessionID string
	Units     float64
}

// HandleUsage implements usage.Plugin.
func (l *UsageLedger) HandleUsage(ctx context.Context, record coreusage.Record) {
	resolved := coreusage.Resolve(ctx, record)
	l.Add(LedgerEntry{
		At:        resolved.RequestedAt,
		Provider:  resolved.Provider,
		AuthIndex: strings.TrimSpace(resolved.AuthIndex),
		Model:     resolved.Model,
		SessionID: resolved.SessionID,
		Units:     CostUnits(resolved.Detail.TokenBreakdown),
	}, true)
}

// Seed counts stored usage from since on, so a restart keeps the week's history. Seeded
// demand is counted as unthrottled: the store does not know the delays.
func (l *UsageLedger) Seed(since time.Time, entries []LedgerEntry) {
	for _, entry := range entries {
		l.Add(entry, false)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if since.Before(l.since) {
		l.since = since
	}
}

// Add counts one request. live requests have their demand scaled up by the provider's
// current throttle speed.
func (l *UsageLedger) Add(entry LedgerEntry, live bool) {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	routing, tracked := quota.RoutingWindowsFor(provider, canonicalModelKey(entry.Model))
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry.SessionID != "" {
		if prev, ok := l.last[entry.SessionID]; ok {
			if gap := entry.At.Sub(prev); gap > 0 && gap <= ledgerSessionGap {
				l.gaps = append(l.gaps, ledgerGap{at: entry.At, gap: gap})
			}
		}
		if prev, ok := l.last[entry.SessionID]; !ok || entry.At.After(prev) {
			l.last[entry.SessionID] = entry.At
		}
	}
	if !tracked || entry.Units <= 0 {
		return
	}
	demand := entry.Units
	if speed, ok := l.speeds[provider]; live && ok && speed > 0 {
		demand /= speed
	}
	for _, window := range spentWindows(routing) {
		if entry.AuthIndex != "" {
			series(l.accounts, ledgerKey{entry.AuthIndex, window}).add(entry.At, entry.Units)
		}
		series(l.demand, ledgerKey{provider, window}).add(entry.At, demand)
	}
}

// spentWindows lists every window a request spends, every alternative of a slot included.
func spentWindows(routing quota.RoutingWindows) []quota.WindowID {
	out := []quota.WindowID{routing.Short}
	for _, slot := range routing.Long {
		out = append(out, slot...)
	}
	return out
}

func series(m map[ledgerKey]*minuteSeries, key ledgerKey) *minuteSeries {
	s := m[key]
	if s == nil {
		s = newMinuteSeries()
		m[key] = s
	}
	return s
}

// SetSpeed records the share of its unthrottled rate provider runs at now.
func (l *UsageLedger) SetSpeed(provider string, speed float64, now time.Time) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.speeds[provider] = speed
	log := l.speedLog[provider]
	if n := len(log); n > 0 && now.Sub(log[n-1].at) < time.Minute {
		log[n-1].speed = speed
	} else {
		log = append(log, speedSample{at: now, speed: speed})
	}
	// The last sample before the span still says what speed the span started at.
	cut := 0
	for cut+1 < len(log) && !log[cut+1].at.After(now.Add(-burnSpan)) {
		cut++
	}
	l.speedLog[provider] = log[cut:]
}

// MeanSpeed is provider's speed averaged over the time of the last burnSpan: each sample
// holds until the next, and before the first one the provider ran at full speed.
func (l *UsageLedger) MeanSpeed(provider string, now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	from := now.Add(-burnSpan)
	at, speed, weighted := from, 1.0, 0.0
	for _, s := range l.speedLog[strings.ToLower(strings.TrimSpace(provider))] {
		if s.at.After(at) {
			weighted += speed * s.at.Sub(at).Hours()
			at = s.at
		}
		speed = s.speed
	}
	weighted += speed * now.Sub(at).Hours()
	return weighted / burnSpan.Hours()
}

// Observe records the share of w used on the account with authIndex, as of w.ObservedAt.
func (l *UsageLedger) Observe(authIndex string, w quota.Window) {
	key := ledgerKey{authIndex, w.ID}
	l.mu.Lock()
	defer l.mu.Unlock()
	log := l.observed[key]
	if n := len(log); n > 0 && !w.ObservedAt.After(log[n-1].at) {
		return
	}
	log = append(log, shareObservation{at: w.ObservedAt, used: 1 - w.Remaining, resetAt: w.ResetAt})
	l.observed[key] = pruneBefore(log, w.ObservedAt.Add(-burnSpan), func(o shareObservation) time.Time { return o.at })
}

// Burn is the share of window id per hour the account with authIndex spent over the last
// burnSpan, from the quota reads alone: no window size is involved. ok is false while the
// reads span less than minBurnSpan.
func (l *UsageLedger) Burn(authIndex string, id quota.WindowID, now time.Time) (float64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var first, last *shareObservation
	for i, o := range l.observed[ledgerKey{authIndex, id}] {
		if o.at.Before(now.Add(-burnSpan)) {
			continue
		}
		if first == nil {
			first = &l.observed[ledgerKey{authIndex, id}][i]
		}
		last = &l.observed[ledgerKey{authIndex, id}][i]
	}
	if first == nil || last.at.Sub(first.at) < minBurnSpan {
		return 0, false
	}
	spent := last.used - first.used
	if !sameWindow(last.resetAt, first.resetAt) {
		// The window reset in between: what is used now was all spent since.
		spent = last.used
	}
	return max(spent, 0) / last.at.Sub(first.at).Hours(), true
}

// sameWindow reports whether two reports of a window's reset are the same window: quota
// reads and response headers give the same reset at different precision.
func sameWindow(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.Sub(b).Abs() < time.Minute
}

// pruneBefore drops the leading items older than cut.
func pruneBefore[T any](items []T, cut time.Time, at func(T) time.Time) []T {
	i := 0
	for i < len(items) && at(items[i]).Before(cut) {
		i++
	}
	return items[i:]
}

// Forecast is provider's demand on window: the last hour for the next hour, the last
// day's hourly average up to a day ahead, the last week's beyond. An average over a span
// the ledger only partly covers is taken over the covered part.
func (l *UsageLedger) Forecast(provider string, window quota.WindowID, now time.Time) Forecast {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.demand[ledgerKey{strings.ToLower(strings.TrimSpace(provider)), window}]
	if s == nil {
		return Forecast{}
	}
	covered := max(now.Sub(l.since), time.Hour)
	average := func(span time.Duration) float64 {
		return s.sum(now.Add(-span), now) / min(span, covered).Hours()
	}
	return Forecast{Hour: s.sum(now.Add(-time.Hour), now), Day: average(24 * time.Hour), Week: average(ledgerSpan)}
}

// Cycle is the average gap between consecutive requests of a session over the last half
// hour: how long one agent turn takes. ok is false without such requests.
func (l *UsageLedger) Cycle(now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-ledgerCycleSpan)
	keep := l.gaps[:0]
	var total time.Duration
	for _, g := range l.gaps {
		if g.at.After(cut) {
			keep = append(keep, g)
			total += g.gap
		}
	}
	l.gaps = keep
	for session, at := range l.last {
		if now.Sub(at) > ledgerSessionGap {
			delete(l.last, session)
		}
	}
	if len(keep) == 0 {
		return 0, false
	}
	return total / time.Duration(len(keep)), true
}

// Capacity estimates how many cost units w holds on the account with authIndex: units
// spent since the anchor ÷ the share of w spent since it. While the current window has
// too little evidence (no pending reset, or less than minCapacityEvidence spent since
// the anchor), the account's last estimate stands; ok is false without one.
func (l *UsageLedger) Capacity(authIndex string, w quota.Window, now time.Time) (float64, bool) {
	key := ledgerKey{authIndex, w.ID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if units, ok := l.estimateLocked(key, w, now); ok {
		l.sized[key] = units
		return units, true
	}
	units, ok := l.sized[key]
	return units, ok
}

func (l *UsageLedger) estimateLocked(key ledgerKey, w quota.Window, now time.Time) (float64, bool) {
	if w.ResetAt.IsZero() || !w.ResetAt.After(now) || w.Length <= 0 {
		return 0, false
	}
	used := 1 - w.Remaining
	anchor, ok := l.anchors[key]
	switch {
	case !ok || !sameWindow(anchor.resetAt, w.ResetAt):
		if start := w.ResetAt.Add(-w.Length); !start.Before(l.since) {
			anchor = capacityAnchor{resetAt: w.ResetAt, at: start}
		} else {
			anchor = capacityAnchor{resetAt: w.ResetAt, at: w.ObservedAt, used: used}
		}
	case used < anchor.lastUsed-0.005:
		// Less used than before in the same window: a reset was spent, and the spend
		// before it says nothing about this window's size.
		anchor = capacityAnchor{resetAt: w.ResetAt, at: w.ObservedAt, used: used}
	}
	anchor.lastUsed = used
	l.anchors[key] = anchor
	spent := used - anchor.used
	// Whole percents subtract inexactly: 1 − 0.9 is just under 0.1.
	if spent < minCapacityEvidence-1e-9 {
		return 0, false
	}
	s := l.accounts[key]
	if s == nil {
		return 0, false
	}
	units := s.sum(anchor.at, w.ObservedAt)
	if units <= 0 {
		return 0, false
	}
	return units / spent, true
}

// medianOf returns the median of values, which must not be empty.
func medianOf(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// minuteSeries holds a week of per-minute sums, with hourly sums so a week-long range
// reads about 300 slots.
type minuteSeries struct {
	minutes []slot
	hours   []slot
}

type slot struct {
	index int64
	value float64
}

const (
	seriesMinutes = int64(ledgerSpan/time.Minute) + 60
	seriesHours   = int64(ledgerSpan/time.Hour) + 2
)

func newMinuteSeries() *minuteSeries {
	return &minuteSeries{minutes: make([]slot, seriesMinutes), hours: make([]slot, seriesHours)}
}

func (s *minuteSeries) add(at time.Time, v float64) {
	addSlot(s.minutes, at.Unix()/60, v)
	addSlot(s.hours, at.Unix()/3600, v)
}

func addSlot(ring []slot, index int64, v float64) {
	sl := &ring[index%int64(len(ring))]
	if sl.index != index {
		*sl = slot{index: index}
	}
	sl.value += v
}

func readSlot(ring []slot, index int64) float64 {
	if sl := ring[index%int64(len(ring))]; sl.index == index {
		return sl.value
	}
	return 0
}

// sum adds the minutes in [from, to), to the minute; ranges older than a week read 0.
func (s *minuteSeries) sum(from, to time.Time) float64 {
	first, end := from.Unix()/60, to.Unix()/60
	if limit := end - int64(ledgerSpan/time.Minute); first < limit {
		first = limit
	}
	total := 0.0
	m := first
	for ; m < end && m%60 != 0; m++ {
		total += readSlot(s.minutes, m)
	}
	for ; m+60 <= end; m += 60 {
		total += readSlot(s.hours, m/60)
	}
	for ; m < end; m++ {
		total += readSlot(s.minutes, m)
	}
	return total
}
