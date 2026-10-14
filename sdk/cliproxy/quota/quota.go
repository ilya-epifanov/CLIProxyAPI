// Package quota keeps a typed, merged view of the quota windows of each
// account. Windows come from two sources: probe responses of the providers'
// usage endpoints and the passive response-header signals stored per account.
// A routing selector reads the merged view on every pick.
//
// The package depends on the standard library and gjson only, because
// sdk/cliproxy/auth imports it.
package quota

import (
	"sort"
	"time"
)

// WindowID names one quota window of a provider.
type WindowID string

const (
	FiveHour      WindowID = "five-hour"       // Claude and Codex 5-hour window
	SevenDay      WindowID = "seven-day"       // Claude shared 7-day window
	SevenDayFable WindowID = "seven-day-fable" // Claude Fable-scoped 7-day window
	Weekly        WindowID = "weekly"          // Codex weekly window
	Monthly       WindowID = "monthly"         // Codex monthly second window (team plans)
)

// Source tells where a window was observed.
type Source string

const (
	SourceProbe   Source = "probe"
	SourceHeaders Source = "headers"
)

// Window is one observed quota window.
type Window struct {
	ID         WindowID
	Remaining  float64       // remaining fraction, clamped to 0..1
	ResetAt    time.Time     // zero: no reset pending (window not started, or reset time not reported)
	Length     time.Duration // window length; > 0
	ObservedAt time.Time
	Source     Source
}

// Snapshot is the merged set of windows of one account. Zero value is empty.
type Snapshot struct {
	windows map[WindowID]Window
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

// Windows returns all windows sorted by ID.
func (s Snapshot) Windows() []Window {
	out := make([]Window, 0, len(s.windows))
	for _, w := range s.windows {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Empty reports whether the snapshot holds no window.
func (s Snapshot) Empty() bool {
	return len(s.windows) == 0
}

// newWindow builds a window from a used percentage (0..100).
func newWindow(id WindowID, usedPercent float64, resetAt time.Time, length time.Duration, observedAt time.Time, source Source) Window {
	return Window{
		ID:         id,
		Remaining:  clampFraction(1 - usedPercent/100),
		ResetAt:    resetAt,
		Length:     length,
		ObservedAt: observedAt,
		Source:     source,
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
