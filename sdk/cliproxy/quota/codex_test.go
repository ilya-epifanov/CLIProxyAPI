package quota

import (
	"testing"
	"time"
)

func TestParseCodexUsage(t *testing.T) {
	const resetPrimary, resetSecondary = 1792015200, 1792368000
	probe := func(id WindowID, remaining float64, resetAt time.Time, length time.Duration) Window {
		return Window{ID: id, Remaining: remaining, ResetAt: resetAt, Length: length, ObservedAt: testObservedAt, Source: SourceProbe}
	}

	tests := []struct {
		name string
		body string
		want []Window
	}{
		{
			name: "plus plan, other limits ignored",
			body: `{
				"plan_type": "plus",
				"rate_limit": {
					"allowed": true,
					"limit_reached": false,
					"primary_window": {"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 7200, "reset_at": 1792015200},
					"secondary_window": {"used_percent": 47.5, "limit_window_seconds": 604800, "reset_after_seconds": 360000, "reset_at": 1792368000}
				},
				"code_review_rate_limit": {
					"allowed": true, "limit_reached": false,
					"primary_window": {"used_percent": 99, "limit_window_seconds": 18000, "reset_at": 1792015200},
					"secondary_window": null
				},
				"additional_rate_limits": [
					{"limit_name": "GPT-5-Codex-Max", "metered_feature": "codex_bengalfox", "rate_limit": {"allowed": false, "limit_reached": true,
						"primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_at": 1792015200}}}
				],
				"credits": {"has_credits": false, "unlimited": false, "balance": "0"}
			}`,
			want: []Window{
				probe(FiveHour, 0.88, unixUTC(resetPrimary), 5*time.Hour),
				probe(Weekly, 0.525, unixUTC(resetSecondary), 168*time.Hour),
			},
		},
		{
			name: "swapped durations classify by seconds",
			body: `{"rate_limit": {
				"primary_window": {"used_percent": 70, "limit_window_seconds": 604800, "reset_at": 1792368000},
				"secondary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_at": 1792015200}
			}}`,
			want: []Window{
				probe(FiveHour, 0.8, unixUTC(resetPrimary), 5*time.Hour),
				probe(Weekly, 0.3, unixUTC(resetSecondary), 168*time.Hour),
			},
		},
		{
			name: "team plan monthly secondary",
			body: `{"plan_type": "team", "rate_limit": {
				"primary_window": {"used_percent": 3, "limit_window_seconds": 18000, "reset_at": 1792015200},
				"secondary_window": {"used_percent": 30, "limit_window_seconds": 2592000, "reset_at": 1792368000}
			}}`,
			want: []Window{
				probe(FiveHour, 0.97, unixUTC(resetPrimary), 5*time.Hour),
				probe(Monthly, 0.7, unixUTC(resetSecondary), 30*24*time.Hour),
			},
		},
		{
			name: "order fallback without durations",
			body: `{"rate_limit": {
				"primary_window": {"used_percent": 40, "reset_after_seconds": 1800},
				"secondary_window": {"used_percent": 10, "reset_after_seconds": 86400}
			}}`,
			want: []Window{
				probe(FiveHour, 0.6, testObservedAt.Add(30*time.Minute), 5*time.Hour),
				probe(Weekly, 0.9, testObservedAt.Add(24*time.Hour), 168*time.Hour),
			},
		},
		{
			name: "weekly-only primary is not reused as five-hour",
			body: `{"rate_limit": {"primary_window": {"used_percent": 55, "limit_window_seconds": 604800, "reset_at": 1792368000}, "secondary_window": null}}`,
			want: []Window{probe(Weekly, 0.45, unixUTC(resetSecondary), 168*time.Hour)},
		},
		{
			name: "limit_reached fills a window with a known reset",
			body: `{"rate_limit": {"allowed": false, "limit_reached": true,
				"primary_window": {"limit_window_seconds": 18000, "reset_at": 1792015200},
				"secondary_window": {"limit_window_seconds": 604800}
			}}`,
			want: []Window{probe(FiveHour, 0, unixUTC(resetPrimary), 5*time.Hour)},
		},
		{
			name: "allowed false alone counts as limit reached",
			body: `{"rate_limit": {"allowed": false,
				"primary_window": {"limit_window_seconds": 18000, "reset_after_seconds": 600}
			}}`,
			want: []Window{probe(FiveHour, 0, testObservedAt.Add(10*time.Minute), 5*time.Hour)},
		},
		{
			name: "missing used percent without limit reached omits the window",
			body: `{"rate_limit": {"allowed": true, "limit_reached": false,
				"primary_window": {"limit_window_seconds": 18000, "reset_at": 1792015200},
				"secondary_window": {"used_percent": "8", "limit_window_seconds": 604800, "reset_at": 1792368000}
			}}`,
			want: []Window{probe(Weekly, 0.92, unixUTC(resetSecondary), 168*time.Hour)},
		},
		{
			name: "reset_at in milliseconds and as ISO string",
			body: `{"rate_limit": {
				"primary_window": {"used_percent": 1, "limit_window_seconds": 18000, "reset_at": 1792015200000},
				"secondary_window": {"used_percent": 2, "limit_window_seconds": 604800, "reset_at": "2026-10-18T09:00:00Z"}
			}}`,
			want: []Window{
				probe(FiveHour, 0.99, unixUTC(resetPrimary), 5*time.Hour),
				probe(Weekly, 0.98, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), 168*time.Hour),
			},
		},
		{
			name: "camelCase variants",
			body: `{"rateLimit": {"allowed": true, "limitReached": true,
				"primaryWindow": {"usedPercent": 33, "limitWindowSeconds": 18000, "resetAfterSeconds": 3600},
				"secondaryWindow": {"limitWindowSeconds": 604800, "resetAt": 1792368000}
			}}`,
			want: []Window{
				probe(FiveHour, 0.67, testObservedAt.Add(time.Hour), 5*time.Hour),
				probe(Weekly, 0, unixUTC(resetSecondary), 168*time.Hour),
			},
		},
		{
			name: "no rate limit",
			body: `{"plan_type": "free", "rate_limit": null}`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCodexUsage([]byte(tt.body), testObservedAt)
			if err != nil {
				t.Fatalf("ParseCodexUsage: %v", err)
			}
			assertWindows(t, got, tt.want)
		})
	}
}

func TestParseCodexUsageInvalid(t *testing.T) {
	for _, body := range []string{``, `{"rate_limit": {`, `[1]`, `null`} {
		if _, err := ParseCodexUsage([]byte(body), testObservedAt); err == nil {
			t.Errorf("body %q: expected error", body)
		}
	}
}
