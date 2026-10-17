package auth

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// RankClass says why a candidate has its rank. Lower classes are picked first.
type RankClass int

const (
	// ClassReady: every long window has quota and the short window is above the burst reserve.
	ClassReady RankClass = iota
	// ClassBurstLow: like ready, but the short window is below the burst reserve, so a new
	// session would likely hit a short-window limit soon and lose its prompt cache on rebind.
	ClassBurstLow
	// ClassUnknown: a long window was never observed, or the provider has no quota windows.
	ClassUnknown
	// ClassExhausted: the short window or a long window is at 0 % and has not reset yet.
	ClassExhausted
)

// String returns the wire name of the class.
func (c RankClass) String() string {
	switch c {
	case ClassReady:
		return "ready"
	case ClassBurstLow:
		return "burst_low"
	case ClassUnknown:
		return "unknown"
	case ClassExhausted:
		return "exhausted"
	default:
		return "invalid"
	}
}

// PaceConfig tunes PaceSelector.
type PaceConfig struct {
	// BurstReserve is the short-window remaining fraction below which a candidate is ClassBurstLow.
	BurstReserve float64
	// PaceTolerance is the top-band width: candidates whose pace is at least
	// (1 - PaceTolerance) × the best pace are picked at random.
	PaceTolerance float64
	// StaleAfter marks a ranking entry stale when its oldest window observation is older.
	StaleAfter time.Duration
	// Throttle slows requests down while the pool spends quota ahead of schedule.
	Throttle ThrottleConfig
}

// minTimeLeft keeps R/T finite just before a reset.
const minTimeLeft = time.Minute

// PaceSelector picks the account that is furthest behind schedule on its long quota
// windows: pace = remaining fraction / fraction of the window's time left, lowest over
// the account's long windows. Accounts whose short window is below the burst reserve
// rank after the others. Wrapped by SessionAffinitySelector, it places sessions, not
// requests: bound sessions keep their account.
type PaceSelector struct {
	cfg  PaceConfig
	view quota.View
	now  func() time.Time
	intn func(n int) int
}

// NewPaceSelector returns a pace selector reading quota from view, which must not be nil.
func NewPaceSelector(cfg PaceConfig, view quota.View) *PaceSelector {
	return &PaceSelector{cfg: cfg, view: view, now: time.Now, intn: rand.IntN}
}

// Ranked is one candidate in pace order.
type Ranked struct {
	Auth *Auth
	// Priority is the account's tier (the upstream priority attribute).
	Priority int
	Class    RankClass
	// LongWindows are the long windows used for this account's selection model, one per
	// slot that the account reports; empty when the provider has no quota windows.
	LongWindows []quota.WindowID
	// Pace is set for ClassReady and ClassBurstLow (> 0) and NaN otherwise. PaceWindow is
	// the long window with the lowest pace, ResetAt its reset (zero when none is pending,
	// in the past when Estimated).
	Pace       float64
	PaceWindow quota.WindowID
	ResetAt    time.Time
	// Estimated means the pace window's reset has passed since it was observed, so the
	// window is assumed refilled until the next probe.
	Estimated bool
	// ShortRemaining is the short window's remaining fraction, NaN when not observed.
	ShortRemaining float64
	// ExhaustedWindow and BackAt are set for ClassExhausted; BackAt is zero when the
	// window reported no reset time.
	ExhaustedWindow quota.WindowID
	BackAt          time.Time
	// TopBand marks the entries a cold pick chooses from at random.
	TopBand bool
	// ObservedAt is the oldest observation among the windows used; Stale compares it with
	// PaceConfig.StaleAfter.
	ObservedAt time.Time
	Stale      bool
}

// Ranker is implemented by selectors that can explain their order. ctx carries the
// selection models (withSelectionModels) as it does for Pick.
type Ranker interface {
	Rank(ctx context.Context, auths []*Auth, model string, now time.Time) []Ranked
}

type selectionModelsKey struct{}

// withSelectionModels attaches auth ID → selection model (Manager.selectionModelForAuth):
// the model each candidate will serve after its prefix and OAuth alias are applied.
func withSelectionModels(ctx context.Context, models map[string]string) context.Context {
	if len(models) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, selectionModelsKey{}, models)
}

// selectionModelFromContext returns the candidate's selection model, or the route model
// when the selector runs outside the Manager.
func selectionModelFromContext(ctx context.Context, auth *Auth, routeModel string) string {
	if ctx != nil {
		if models, _ := ctx.Value(selectionModelsKey{}).(map[string]string); models != nil {
			if model, ok := models[auth.ID]; ok {
				return model
			}
		}
	}
	return routeModel
}

// Pick returns a random top-band candidate.
func (s *PaceSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ranked := s.Rank(ctx, available, model, now)
	n := 1
	for n < len(ranked) && ranked[n].TopBand {
		n++
	}
	return ranked[s.intn(n)].Auth, nil
}

// Rank orders auths by class, then by pace (ready, burst low), by return time
// (exhausted) or by ID (unknown), and marks the top band.
func (s *PaceSelector) Rank(ctx context.Context, auths []*Auth, model string, now time.Time) []Ranked {
	out := make([]Ranked, 0, len(auths))
	for _, auth := range auths {
		out = append(out, s.classify(auth, selectionModelFromContext(ctx, auth, model), now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.Class != y.Class {
			return x.Class < y.Class
		}
		switch x.Class {
		case ClassReady, ClassBurstLow:
			if x.Pace != y.Pace {
				return x.Pace > y.Pace
			}
		case ClassExhausted:
			if bx, by := zeroLast(x.BackAt), zeroLast(y.BackAt); !bx.Equal(by) {
				return bx.Before(by)
			}
		}
		return x.Auth.ID < y.Auth.ID
	})
	markTopBand(out, s.cfg.PaceTolerance)
	return out
}

// classify ranks one account against the windows of its selection model, which is the
// model the account will serve after its alias, not the route model.
func (s *PaceSelector) classify(auth *Auth, selectionModel string, now time.Time) Ranked {
	r := Ranked{Auth: auth, Priority: authPriority(auth), Class: ClassUnknown, Pace: math.NaN(), ShortRemaining: math.NaN()}
	windows, ok := s.windowsOf(auth, selectionModel)
	if !ok {
		return r
	}
	short, hasShort, long := windows.short, windows.hasShort, windows.long
	for _, w := range long {
		r.LongWindows = append(r.LongWindows, w.ID)
	}
	for _, w := range windows.all() {
		if exhausted(w, now) {
			r.Class, r.ExhaustedWindow, r.BackAt = ClassExhausted, w.ID, w.ResetAt
			return r
		}
	}
	if !windows.complete {
		return r
	}
	r.Pace = math.Inf(1)
	for _, w := range long {
		p, estimated := pace(w, now)
		if estimated {
			s.view.RequestRefresh(auth.ID)
		}
		if p < r.Pace {
			r.Pace, r.PaceWindow, r.ResetAt, r.Estimated = p, w.ID, w.ResetAt, estimated
		}
		r.ObservedAt = oldest(r.ObservedAt, w.ObservedAt)
	}
	r.Class = ClassReady
	if hasShort {
		r.ShortRemaining = short.Remaining
		r.ObservedAt = oldest(r.ObservedAt, short.ObservedAt)
		if short.Remaining < s.cfg.BurstReserve && short.ResetAt.After(now) {
			r.Class = ClassBurstLow
		}
	}
	r.Stale = s.cfg.StaleAfter > 0 && now.Sub(r.ObservedAt) > s.cfg.StaleAfter
	return r
}

// accountWindows are the observed routing windows of one account's selection model.
type accountWindows struct {
	short    quota.Window
	hasShort bool
	// long holds the first observed window of each long slot, in slot order.
	long []quota.Window
	// complete is false when a long slot has no observation.
	complete bool
}

// all returns the short window, when observed, followed by the long ones.
func (w accountWindows) all() []quota.Window {
	if !w.hasShort {
		return w.long
	}
	return append([]quota.Window{w.short}, w.long...)
}

// windowsOf looks up auth's routing windows for its selection model. ok is false when the
// provider has no quota windows or none was observed.
func (s *PaceSelector) windowsOf(auth *Auth, selectionModel string) (accountWindows, bool) {
	routing, tracked := quota.RoutingWindowsFor(auth.Provider, canonicalModelKey(selectionModel))
	if !tracked {
		return accountWindows{}, false
	}
	// auth is a candidate clone, so reading its signals here is safe.
	snapshot, ok := s.view.Lookup(auth.ID, auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt)
	if !ok {
		return accountWindows{}, false
	}
	out := accountWindows{long: make([]quota.Window, 0, len(routing.Long))}
	out.short, out.hasShort = snapshot.Window(routing.Short)
	for _, slot := range routing.Long {
		if w, found := snapshot.First(slot...); found {
			out.long = append(out.long, w)
		}
	}
	out.complete = len(out.long) == len(routing.Long)
	return out, true
}

// exhausted reports a window at 0 % whose reset is pending or unknown.
func exhausted(w quota.Window, now time.Time) bool {
	return w.Remaining <= 0 && (w.ResetAt.IsZero() || w.ResetAt.After(now))
}

// pace returns R/T for one window that is not exhausted.
func pace(w quota.Window, now time.Time) (p float64, estimated bool) {
	remaining, timeLeft := windowShare(w, now)
	return remaining / timeLeft, !w.ResetAt.IsZero() && !w.ResetAt.After(now)
}

// windowShare returns a window's remaining fraction R and the fraction T of its length
// left before the reset, both in 0..1 with T > 0.
func windowShare(w quota.Window, now time.Time) (remaining, timeLeft float64) {
	switch {
	case w.ResetAt.IsZero(): // no reset pending: the window's clock starts on use, T = 1
		return w.Remaining, 1
	case !w.ResetAt.After(now): // reset passed: refilled since the last observation
		return 1, 1
	}
	t := float64(max(w.ResetAt.Sub(now), minTimeLeft)) / float64(w.Length)
	return w.Remaining, min(t, 1)
}

func markTopBand(ranked []Ranked, tolerance float64) {
	for i := range ranked {
		if ranked[i].Class != ranked[0].Class {
			return
		}
		switch ranked[0].Class {
		case ClassReady, ClassBurstLow:
			ranked[i].TopBand = ranked[i].Pace >= (1-tolerance)*ranked[0].Pace
		case ClassUnknown:
			ranked[i].TopBand = true
		default:
			ranked[i].TopBand = i == 0
		}
	}
}

// zeroLast maps an unknown (zero) time after every known one.
func zeroLast(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(math.MaxInt32, 0)
	}
	return t
}

func oldest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
