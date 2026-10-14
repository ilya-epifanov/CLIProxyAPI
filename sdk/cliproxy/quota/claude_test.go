package quota

import (
	"testing"
	"time"
)

func TestParseClaudeUsage(t *testing.T) {
	fiveHourReset := time.Date(2026, 10, 13, 17, 0, 0, 123456000, time.UTC)
	sevenDayReset := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	fableReset := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	probe := func(id WindowID, remaining float64, resetAt time.Time, length time.Duration) Window {
		return Window{ID: id, Remaining: remaining, ResetAt: resetAt, Length: length, ObservedAt: testObservedAt, Source: SourceProbe}
	}

	tests := []struct {
		name string
		body string
		want []Window
	}{
		{
			name: "named windows and active Fable limit",
			body: `{
				"five_hour": {"utilization": 42.0, "resets_at": "2026-10-13T17:00:00.123456+00:00"},
				"seven_day": {"utilization": "17", "resets_at": "2026-10-18T09:00:00+00:00"},
				"seven_day_opus": null,
				"seven_day_sonnet": {"utilization": 3, "resets_at": "2026-10-18T09:00:00+00:00"},
				"iguana_necktie": {"utilization": 5, "resets_at": "2026-10-20T00:00:00+00:00"},
				"extra_usage": {"is_enabled": false, "monthly_limit": null},
				"limits": [
					{"kind": "session", "group": "session", "percent": 42, "resets_at": "2026-10-13T17:00:00+00:00", "is_active": true, "scope": null},
					{"kind": "weekly_scoped", "group": "weekly", "percent": 90, "resets_at": "2026-10-25T10:00:00+00:00", "is_active": false, "scope": {"model": {"id": null, "display_name": "Fable"}}},
					{"kind": "WEEKLY_SCOPED", "group": "weekly", "percent": 64, "resets_at": "2026-10-19T10:00:00.000000+00:00", "is_active": true, "scope": {"model": {"id": null, "display_name": "Fable 5"}}}
				]
			}`,
			want: []Window{
				probe(FiveHour, 0.58, fiveHourReset, 5*time.Hour),
				probe(SevenDay, 0.83, sevenDayReset, 168*time.Hour),
				probe(SevenDayFable, 0.36, fableReset, 168*time.Hour),
			},
		},
		{
			name: "first Fable limit when none is active",
			body: `{"limits": [
				{"kind": "weekly_scoped", "percent": null, "resets_at": null, "is_active": true, "scope": {"model": {"display_name": "Fable"}}},
				{"kind": "weekly_scoped", "percent": "25", "resets_at": "2026-10-19T10:00:00Z", "is_active": false, "scope": {"model": {"display_name": "fable"}}},
				{"kind": "weekly_scoped", "percent": 80, "resets_at": "2026-10-25T10:00:00Z", "is_active": false, "scope": {"model": {"display_name": "Fable"}}},
				{"kind": "weekly_scoped", "percent": 99, "is_active": true, "scope": {"model": {"display_name": "Opus"}}}
			]}`,
			want: []Window{probe(SevenDayFable, 0.75, fableReset, 168*time.Hour)},
		},
		{
			name: "idle five-hour window",
			body: `{"five_hour": {"utilization": 0, "resets_at": null}, "seven_day": {"utilization": 12, "resets_at": "2026-10-18T09:00:00Z"}}`,
			want: []Window{
				probe(FiveHour, 1, time.Time{}, 5*time.Hour),
				probe(SevenDay, 0.88, sevenDayReset, 168*time.Hour),
			},
		},
		{
			name: "Fable from iguana_necktie",
			body: `{"five_hour": null, "iguana_necktie": {"utilization": "41.5", "resets_at": "2026-10-19T10:00:00+00:00"}, "limits": []}`,
			want: []Window{probe(SevenDayFable, 0.585, fableReset, 168*time.Hour)},
		},
		{
			name: "dollar-denominated iguana_necktie is not Fable",
			body: `{
				"seven_day": {"utilization": 9, "resets_at": "2026-10-18T09:00:00Z"},
				"iguana_necktie": {"utilization": 5.04, "resets_at": "2026-11-05T07:59:00+00:00", "limit_dollars": 100, "used_dollars": 5.04, "remaining_dollars": 94.96},
				"limits": [{"kind": "weekly_all", "percent": 9, "resets_at": "2026-10-18T09:00:00Z", "is_active": false, "scope": null}]
			}`,
			want: []Window{probe(SevenDay, 0.91, sevenDayReset, 168*time.Hour)},
		},
		{
			name: "remaining_dollars alone marks a credit pool",
			body: `{"iguana_necktie": {"utilization": 5, "resets_at": null, "remaining_dollars": "95"}}`,
			want: nil,
		},
		{
			name: "missing or non-numeric utilization omits the window",
			body: `{"five_hour": {"resets_at": "2026-10-13T17:00:00Z"}, "seven_day": {"utilization": "n/a", "resets_at": null}, "iguana_necktie": {"utilization": null}}`,
			want: nil,
		},
		{
			name: "utilization above 100 clamps to zero remaining",
			body: `{"five_hour": {"utilization": 104, "resets_at": "2026-10-13T17:00:00.123456Z"}}`,
			want: []Window{probe(FiveHour, 0, fiveHourReset, 5*time.Hour)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseClaudeUsage([]byte(tt.body), testObservedAt)
			if err != nil {
				t.Fatalf("ParseClaudeUsage: %v", err)
			}
			assertWindows(t, got, tt.want)
		})
	}
}

func TestParseClaudeUsageInvalid(t *testing.T) {
	for _, body := range []string{``, `{"five_hour":`, `[]`, `"text"`} {
		if _, err := ParseClaudeUsage([]byte(body), testObservedAt); err == nil {
			t.Errorf("body %q: expected error", body)
		}
	}
}
