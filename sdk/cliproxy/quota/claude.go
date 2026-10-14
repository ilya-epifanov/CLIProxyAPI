package quota

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// claudeHeaderWindows lists the unified rate-limit header prefixes Claude
// reports. The Fable window (the 7d_oi headers) is taken from probes only.
var claudeHeaderWindows = []struct {
	prefix string
	id     WindowID
	length time.Duration
}{
	{"anthropic-ratelimit-unified-5h-", FiveHour, fiveHourLength},
	{"anthropic-ratelimit-unified-7d-", SevenDay, weekLength},
}

// parseClaudeHeaders reads utilization fractions (0..1) and Unix-seconds resets
// from lower-cased header signals.
func parseClaudeHeaders(signals map[string]string, observedAt time.Time) []Window {
	var out []Window
	for _, spec := range claudeHeaderWindows {
		utilization, ok := parseNumber(signals[spec.prefix+"utilization"])
		if !ok {
			continue
		}
		resetAt := headerResetAt(signals[spec.prefix+"reset"])
		out = append(out, newWindow(spec.id, utilization*100, resetAt, spec.length, observedAt, SourceHeaders))
	}
	return out
}

// ParseClaudeUsage parses the body of GET api.anthropic.com/api/oauth/usage.
// It mirrors the panel's buildClaudeQuotaWindows for the windows routing uses.
func ParseClaudeUsage(body []byte, observedAt time.Time) ([]Window, error) {
	root, err := parseJSONObject("claude", body)
	if err != nil {
		return nil, err
	}
	var out []Window
	if w, ok := claudeUsageWindow(root.Get("five_hour"), FiveHour, fiveHourLength, observedAt); ok {
		out = append(out, w)
	}
	if w, ok := claudeUsageWindow(root.Get("seven_day"), SevenDay, weekLength, observedAt); ok {
		out = append(out, w)
	}
	if limit, ok := findFableUsageLimit(root.Get("limits")); ok {
		used, _ := jsonNumber(limit.Get("percent"))
		resetAt := jsonResetAt(limit.Get("resets_at"))
		out = append(out, newWindow(SevenDayFable, used, resetAt, weekLength, observedAt, SourceProbe))
	} else if legacy := root.Get("iguana_necktie"); !isDollarDenominated(legacy) {
		// A dollar-denominated iguana_necktie is a credit pool, not the Fable window.
		if w, ok := claudeUsageWindow(legacy, SevenDayFable, weekLength, observedAt); ok {
			out = append(out, w)
		}
	}
	return out, nil
}

// claudeUsageWindow reads {utilization: 0..100, resets_at: ISO-8601 | null}.
func claudeUsageWindow(obj gjson.Result, id WindowID, length time.Duration, observedAt time.Time) (Window, bool) {
	if !obj.IsObject() {
		return Window{}, false
	}
	used, ok := jsonNumber(obj.Get("utilization"))
	if !ok {
		return Window{}, false
	}
	return newWindow(id, used, jsonResetAt(obj.Get("resets_at")), length, observedAt, SourceProbe), true
}

// findFableUsageLimit picks the weekly_scoped Fable entry of limits[] with a
// numeric percent, preferring the active one.
func findFableUsageLimit(limits gjson.Result) (gjson.Result, bool) {
	if !limits.IsArray() {
		return gjson.Result{}, false
	}
	var first gjson.Result
	found := false
	for _, limit := range limits.Array() {
		kind := strings.ToLower(strings.TrimSpace(limit.Get("kind").String()))
		model := strings.ToLower(strings.TrimSpace(limit.Get("scope.model.display_name").String()))
		if kind != "weekly_scoped" || (model != "fable" && model != "fable 5") {
			continue
		}
		if _, ok := jsonNumber(limit.Get("percent")); !ok {
			continue
		}
		if limit.Get("is_active").Type == gjson.True {
			return limit, true
		}
		if !found {
			first, found = limit, true
		}
	}
	return first, found
}

func isDollarDenominated(obj gjson.Result) bool {
	for _, key := range []string{"limit_dollars", "used_dollars", "remaining_dollars"} {
		if _, ok := jsonNumber(obj.Get(key)); ok {
			return true
		}
	}
	return false
}
