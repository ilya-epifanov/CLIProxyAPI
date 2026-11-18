package quota

import "strings"

// Slot lists alternative windows; the first one present in a Snapshot is used.
type Slot []WindowID

// RoutingWindows names the windows a pacing selector uses for one provider.
type RoutingWindows struct {
	Short WindowID
	Long  []Slot // never empty for a tracked provider
}

// RoutingWindowsFor returns the windows a pacing selector uses for a provider and
// the model the account will serve (already alias-resolved by the caller):
//
//	claude: Short five-hour; Long [seven-day], plus [seven-day-fable] when IsFableModel(model)
//	codex:  Short five-hour; Long [weekly, monthly] (one slot with two alternatives)
//
// ok=false for any other provider. Provider match is case-insensitive and trimmed.
func RoutingWindowsFor(provider, model string) (RoutingWindows, bool) {
	switch normalizeProvider(provider) {
	case "claude":
		long := []Slot{{SevenDay}}
		if IsFableModel(model) {
			long = append(long, Slot{SevenDayFable})
		}
		return RoutingWindows{Short: FiveHour, Long: long}, true
	case "codex":
		return RoutingWindows{Short: FiveHour, Long: []Slot{{Weekly, Monthly}}}, true
	default:
		return RoutingWindows{}, false
	}
}

// IsFableModel reports whether model (trimmed, lower-cased) starts with "claude-fable".
func IsFableModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-fable")
}

// AccountWindows are the routing windows observed for one account.
type AccountWindows struct {
	Short    Window
	HasShort bool
	// Long holds the first observed window of each long slot, in slot order.
	Long []Window
	// Complete is false when a long slot has no observation.
	Complete bool
}

// All returns the short window, when observed, followed by the long ones.
func (w AccountWindows) All() []Window {
	if !w.HasShort {
		return w.Long
	}
	return append([]Window{w.Short}, w.Long...)
}

// Observed picks r's windows out of s. A missing short window leaves the account
// uncapped by it; a missing long window leaves the account's quota unknown
// (Complete false).
func (r RoutingWindows) Observed(s Snapshot) AccountWindows {
	out := AccountWindows{Long: make([]Window, 0, len(r.Long))}
	out.Short, out.HasShort = s.Window(r.Short)
	for _, slot := range r.Long {
		if w, found := s.First(slot...); found {
			out.Long = append(out.Long, w)
		}
	}
	out.Complete = len(out.Long) == len(r.Long)
	return out
}
