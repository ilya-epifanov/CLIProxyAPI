// Package quota keeps a typed, merged view of the quota windows of each
// account. Windows come from two sources: probe responses of the providers'
// usage endpoints and the passive response-header signals stored per account.
// A routing selector reads the merged view on every pick.
//
// The package depends on the standard library and moego/ojson only, because
// sdk/cliproxy/auth imports it.
package quota

import "time"

// WindowID names one quota window of a provider.
type WindowID string

const (
	FiveHour      WindowID = "five-hour"       // Claude and Codex 5-hour window
	SevenDay      WindowID = "seven-day"       // Claude shared 7-day window
	SevenDayFable WindowID = "seven-day-fable" // Claude Fable-scoped 7-day window
	Weekly        WindowID = "weekly"          // Codex weekly window
	Monthly       WindowID = "monthly"         // Codex monthly second window (team plans)
)

// Window is one observed quota window.
type Window struct {
	ID         WindowID
	Remaining  float64       // remaining fraction, clamped to 0..1
	ResetAt    time.Time     // zero: no reset pending (window not started, or reset time not reported)
	Length     time.Duration // window length; > 0
	ObservedAt time.Time
}

// Snapshot is the merged set of windows of one account. Zero value is empty.
type Snapshot struct {
	windows map[WindowID]Window
}

// NewSnapshot holds windows; of two with the same ID, the later one is kept.
func NewSnapshot(windows []Window) Snapshot {
	out := Snapshot{windows: make(map[WindowID]Window, len(windows))}
	for _, w := range windows {
		out.windows[w.ID] = w
	}
	return out
}

// Window returns the window with the given ID.
func (s Snapshot) Window(id WindowID) (Window, bool) {
	w, ok := s.windows[id]
	return w, ok
}

// First returns the window of the first id present in the snapshot.
func (s Snapshot) First(ids ...WindowID) (Window, bool) {
	for _, id := range ids {
		if w, ok := s.windows[id]; ok {
			return w, true
		}
	}
	return Window{}, false
}

// newWindow builds a window from a used percentage (0..100).
func newWindow(id WindowID, usedPercent float64, resetAt time.Time, length time.Duration, observedAt time.Time) Window {
	return Window{
		ID:         id,
		Remaining:  clampFraction(1 - usedPercent/100),
		ResetAt:    resetAt,
		Length:     length,
		ObservedAt: observedAt,
	}
}

func clampFraction(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
