package quota

import (
	"time"

	"github.com/tidwall/gjson"
)

// codexRawWindow is one Codex primary or secondary window before classification.
// Headers and probe bodies both reduce to it.
type codexRawWindow struct {
	usedPercent float64
	hasUsed     bool
	length      time.Duration // zero: not reported
	resetAt     time.Time
}

// kind classifies the window by its reported length; "" when unknown.
func (w *codexRawWindow) kind() WindowID {
	switch {
	case w.length == fiveHourLength:
		return FiveHour
	case w.length == weekLength:
		return Weekly
	case w.length >= minMonthLength && w.length <= maxMonthLength:
		return Monthly
	default:
		return ""
	}
}

// pickCodexWindows assigns primary and secondary to the short (five-hour) and
// long (weekly or monthly) slots, mirroring the panel's pickClassifiedWindows:
// classify by length first, then fall back to primary/secondary order for
// windows without a recognized length.
func pickCodexWindows(primary, secondary *codexRawWindow) (short, long *codexRawWindow) {
	for _, w := range []*codexRawWindow{primary, secondary} {
		if w == nil {
			continue
		}
		switch w.kind() {
		case FiveHour:
			if short == nil {
				short = w
			}
		case Weekly, Monthly:
			if long == nil {
				long = w
			}
		}
	}
	if short == nil && primary != nil && primary != long {
		short = primary
	}
	if long == nil && secondary != nil && secondary != short {
		long = secondary
	}
	return short, long
}

// codexWindows classifies primary and secondary and builds the windows. When
// limitReached is set, a window without a used percentage but with a known
// reset counts as fully used.
func codexWindows(primary, secondary *codexRawWindow, limitReached bool, observedAt time.Time, source Source) []Window {
	short, long := pickCodexWindows(primary, secondary)
	var out []Window
	add := func(w *codexRawWindow, id WindowID, defaultLength time.Duration) {
		if w == nil {
			return
		}
		used := w.usedPercent
		if !w.hasUsed {
			if !limitReached || w.resetAt.IsZero() {
				return
			}
			used = 100
		}
		length := w.length
		if length <= 0 {
			length = defaultLength
		}
		out = append(out, newWindow(id, used, w.resetAt, length, observedAt, source))
	}
	add(short, FiveHour, fiveHourLength)
	if long != nil {
		id := Weekly
		if long.kind() == Monthly {
			id = Monthly
		}
		add(long, id, weekLength)
	}
	return out
}

// parseCodexHeaders reads the x-codex-primary-* and x-codex-secondary-*
// signals. Additional and code-review namespaces are ignored.
func parseCodexHeaders(signals map[string]string, observedAt time.Time) []Window {
	return codexWindows(
		codexHeaderWindow(signals, "x-codex-primary-", observedAt),
		codexHeaderWindow(signals, "x-codex-secondary-", observedAt),
		false, observedAt, SourceHeaders,
	)
}

func codexHeaderWindow(signals map[string]string, prefix string, observedAt time.Time) *codexRawWindow {
	usedRaw, hasUsedKey := signals[prefix+"used-percent"]
	minutesRaw, hasMinutes := signals[prefix+"window-minutes"]
	resetAtRaw, hasResetAt := signals[prefix+"reset-at"]
	resetAfterRaw, hasResetAfter := signals[prefix+"reset-after-seconds"]
	if !hasUsedKey && !hasMinutes && !hasResetAt && !hasResetAfter {
		return nil
	}
	w := &codexRawWindow{}
	w.usedPercent, w.hasUsed = parseNumber(usedRaw)
	if minutes, ok := parseNumber(minutesRaw); ok && minutes > 0 {
		w.length = time.Duration(minutes * float64(time.Minute))
	}
	w.resetAt = headerResetAt(resetAtRaw)
	if w.resetAt.IsZero() {
		if seconds, ok := parseNumber(resetAfterRaw); ok {
			w.resetAt = afterSeconds(observedAt, seconds)
		}
	}
	return w
}

// ParseCodexUsage parses the body of GET chatgpt.com/backend-api/wham/usage.
// It reads the main rate_limit only, mirroring the panel's
// buildCodexQuotaWindows, and accepts the camelCase variants the panel accepts.
func ParseCodexUsage(body []byte, observedAt time.Time) ([]Window, error) {
	root, err := parseJSONObject("codex", body)
	if err != nil {
		return nil, err
	}
	rateLimit := field(root, "rate_limit", "rateLimit")
	if !rateLimit.IsObject() {
		return nil, nil
	}
	limitReached := field(rateLimit, "limit_reached", "limitReached").Type == gjson.True ||
		rateLimit.Get("allowed").Type == gjson.False
	return codexWindows(
		codexUsageWindow(field(rateLimit, "primary_window", "primaryWindow"), observedAt),
		codexUsageWindow(field(rateLimit, "secondary_window", "secondaryWindow"), observedAt),
		limitReached, observedAt, SourceProbe,
	), nil
}

func codexUsageWindow(obj gjson.Result, observedAt time.Time) *codexRawWindow {
	if !obj.IsObject() {
		return nil
	}
	w := &codexRawWindow{}
	w.usedPercent, w.hasUsed = jsonNumber(field(obj, "used_percent", "usedPercent"))
	if seconds, ok := jsonNumber(field(obj, "limit_window_seconds", "limitWindowSeconds")); ok && seconds > 0 {
		w.length = time.Duration(seconds * float64(time.Second))
	}
	w.resetAt = jsonResetAt(field(obj, "reset_at", "resetAt"))
	if w.resetAt.IsZero() {
		if seconds, ok := jsonNumber(field(obj, "reset_after_seconds", "resetAfterSeconds")); ok {
			w.resetAt = afterSeconds(observedAt, seconds)
		}
	}
	return w
}
