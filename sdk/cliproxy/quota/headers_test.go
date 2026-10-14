package quota

import (
	"testing"
	"time"
)

func TestParseHeaderSignalsClaude(t *testing.T) {
	const reset5h, reset7d = 1792015200, 1792368000
	tests := []struct {
		name    string
		signals map[string]string
		want    []Window
	}{
		{
			name: "five-hour and seven-day fractions",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-Status":               "allowed",
				"Anthropic-Ratelimit-Unified-5h-Status":            "allowed",
				"Anthropic-Ratelimit-Unified-5h-Utilization":       "0.42",
				"Anthropic-Ratelimit-Unified-5h-Reset":             "1792015200",
				"Anthropic-Ratelimit-Unified-7d-Status":            "allowed",
				"Anthropic-Ratelimit-Unified-7d-Utilization":       "0.9",
				"Anthropic-Ratelimit-Unified-7d-Reset":             "1792368000",
				"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.58, ResetAt: unixUTC(reset5h), Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
				{ID: SevenDay, Remaining: 0.1, ResetAt: unixUTC(reset7d), Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "lower-case keys",
			signals: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "0",
				"anthropic-ratelimit-unified-5h-reset":       "1792015200",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 1, ResetAt: unixUTC(reset5h), Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "7d_oi headers are ignored",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.3",
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":       "1792368000",
			},
			want: nil,
		},
		{
			name: "garbage utilization skipped, garbage reset gives zero reset, overuse clamps",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "high",
				"Anthropic-Ratelimit-Unified-5h-Reset":       "1792015200",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "1.25",
				"Anthropic-Ratelimit-Unified-7d-Reset":       "soon",
			},
			want: []Window{
				{ID: SevenDay, Remaining: 0, Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name:    "codex headers on a claude account",
			signals: map[string]string{"X-Codex-Primary-Used-Percent": "50"},
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertWindows(t, ParseHeaderSignals(" Claude", tt.signals, testObservedAt), tt.want)
		})
	}
}

func TestParseHeaderSignalsCodex(t *testing.T) {
	const resetPrimary, resetSecondary = 1792015200, 1792368000
	tests := []struct {
		name    string
		signals map[string]string
		want    []Window
	}{
		{
			name: "classified by minutes, reset-at and reset-after",
			signals: map[string]string{
				"X-Codex-Plan-Type":                            "plus",
				"X-Codex-Active-Limit":                         "codex",
				"X-Codex-Primary-Used-Percent":                 "25",
				"X-Codex-Primary-Window-Minutes":               "300",
				"X-Codex-Primary-Reset-At":                     "1792015200",
				"X-Codex-Secondary-Used-Percent":               "60",
				"X-Codex-Secondary-Window-Minutes":             "10080",
				"X-Codex-Secondary-Reset-After-Seconds":        "3600",
				"X-Codex-Primary-Over-Secondary-Limit-Percent": "0",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.75, ResetAt: unixUTC(resetPrimary), Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
				{ID: Weekly, Remaining: 0.4, ResetAt: testObservedAt.Add(time.Hour), Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "reset-at wins over reset-after",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "10",
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-At":            "1792015200",
				"X-Codex-Primary-Reset-After-Seconds": "60",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.9, ResetAt: unixUTC(resetPrimary), Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "monthly secondary on a team plan",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     "5",
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Secondary-Used-Percent":   "30",
				"X-Codex-Secondary-Window-Minutes": "43200",
				"X-Codex-Secondary-Reset-At":       "1792368000",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.95, Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
				{ID: Monthly, Remaining: 0.7, ResetAt: unixUTC(resetSecondary), Length: 30 * 24 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "swapped durations classify by minutes",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     "70",
				"X-Codex-Primary-Window-Minutes":   "10080",
				"X-Codex-Secondary-Used-Percent":   "20",
				"X-Codex-Secondary-Window-Minutes": "300",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.8, Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
				{ID: Weekly, Remaining: 0.3, Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "order fallback without minutes, lower-case keys",
			signals: map[string]string{
				"x-codex-primary-used-percent":   "40",
				"x-codex-secondary-used-percent": "100",
			},
			want: []Window{
				{ID: FiveHour, Remaining: 0.6, Length: 5 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
				{ID: Weekly, Remaining: 0, Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name: "garbage used percent skipped; other namespaces ignored",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":                  "n/a",
				"X-Codex-Primary-Window-Minutes":                "300",
				"X-Codex-Secondary-Used-Percent":                "12.5",
				"X-Codex-Secondary-Window-Minutes":              "10080",
				"X-Codex-Bengalfox-Primary-Used-Percent":        "99",
				"X-Codex-Additional-Gpt-5-Primary-Used-Percent": "99",
				"X-Codex-Code-Review-Primary-Used-Percent":      "99",
				"X-Codex-Code-Review-Primary-Window-Minutes":    "300",
			},
			want: []Window{
				{ID: Weekly, Remaining: 0.875, Length: 168 * time.Hour, ObservedAt: testObservedAt, Source: SourceHeaders},
			},
		},
		{
			name:    "claude headers on a codex account",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5"},
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertWindows(t, ParseHeaderSignals("codex", tt.signals, testObservedAt), tt.want)
		})
	}
}

func TestParseHeaderSignalsOtherProvider(t *testing.T) {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5",
		"X-Codex-Primary-Used-Percent":               "50",
	}
	for _, provider := range []string{"gemini", "devin", ""} {
		if got := ParseHeaderSignals(provider, signals, testObservedAt); got != nil {
			t.Errorf("provider %q: got %+v, want nil", provider, got)
		}
	}
}
