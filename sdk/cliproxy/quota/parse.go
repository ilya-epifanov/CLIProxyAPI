package quota

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	fiveHourLength = 5 * time.Hour
	weekLength     = 7 * 24 * time.Hour
	minMonthLength = 28 * 24 * time.Hour
	maxMonthLength = 31 * 24 * time.Hour

	// unixMillisThreshold separates Unix seconds from Unix milliseconds, as the
	// panel's parseUnixToMs does: seconds stay below 1e11 until the year 5138.
	unixMillisThreshold = 1e11
)

// ParseHeaderSignals parses the passive quota signals stored for an account.
// Keys match case-insensitively. Providers other than claude and codex give nil.
func ParseHeaderSignals(provider string, signals map[string]string, observedAt time.Time) []Window {
	if len(signals) == 0 {
		return nil
	}
	lower := make(map[string]string, len(signals))
	for key, value := range signals {
		lower[strings.ToLower(strings.TrimSpace(key))] = value
	}
	switch normalizeProvider(provider) {
	case "claude":
		return parseClaudeHeaders(lower, observedAt)
	case "codex":
		return parseCodexHeaders(lower, observedAt)
	default:
		return nil
	}
}

// ParseProbe parses a usage probe body of provider ("claude" or "codex").
func ParseProbe(provider string, body []byte, observedAt time.Time) ([]Window, error) {
	switch normalizeProvider(provider) {
	case "claude":
		return ParseClaudeUsage(body, observedAt)
	case "codex":
		return ParseCodexUsage(body, observedAt)
	default:
		return nil, fmt.Errorf("quota: unsupported probe provider %q", provider)
	}
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// parseJSONObject validates body and returns its top-level object.
func parseJSONObject(provider string, body []byte) (gjson.Result, error) {
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, fmt.Errorf("quota: parse %s usage: invalid JSON", provider)
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return gjson.Result{}, fmt.Errorf("quota: parse %s usage: top-level value is not an object", provider)
	}
	return root, nil
}

// field returns the first of keys that is present and not null.
func field(obj gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if r := obj.Get(key); r.Exists() && r.Type != gjson.Null {
			return r
		}
	}
	return gjson.Result{}
}

// parseNumber parses a finite number from trimmed text.
func parseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// jsonNumber reads a JSON number or a numeric string.
func jsonNumber(r gjson.Result) (float64, bool) {
	switch r.Type {
	case gjson.Number:
		return r.Num, !math.IsNaN(r.Num) && !math.IsInf(r.Num, 0)
	case gjson.String:
		return parseNumber(r.Str)
	default:
		return 0, false
	}
}

// unixTime converts positive Unix seconds or milliseconds to a UTC time.
func unixTime(v float64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v >= unixMillisThreshold {
		v /= 1000
	}
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
}

// jsonResetAt reads a reset instant given as an ISO-8601 string or as Unix
// seconds or milliseconds, mirroring the panel's resolveResetMs. Anything else
// gives the zero time.
func jsonResetAt(r gjson.Result) time.Time {
	if r.Type == gjson.String {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(r.Str)); err == nil {
			return t.UTC()
		}
	}
	if v, ok := jsonNumber(r); ok {
		if t, ok := unixTime(v); ok {
			return t
		}
	}
	return time.Time{}
}

// headerResetAt reads a Unix-seconds reset header; invalid gives the zero time.
func headerResetAt(value string) time.Time {
	v, ok := parseNumber(value)
	if !ok {
		return time.Time{}
	}
	t, _ := unixTime(v)
	return t
}

// afterSeconds returns observedAt plus a positive offset in seconds; a
// non-positive offset gives the zero time.
func afterSeconds(observedAt time.Time, seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return observedAt.Add(time.Duration(seconds * float64(time.Second)))
}
