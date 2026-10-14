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
