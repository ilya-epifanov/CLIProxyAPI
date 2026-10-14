package quota

import (
	"math"
	"sort"
	"testing"
	"time"
)

var testObservedAt = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func unixUTC(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// assertWindows compares windows by ID, ignoring order.
func assertWindows(t *testing.T, got, want []Window) {
	t.Helper()
	sortWindows := func(ws []Window) []Window {
		out := append([]Window(nil), ws...)
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	got, want = sortWindows(got), sortWindows(want)
	if len(got) != len(want) {
		t.Fatalf("got %d windows %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || math.Abs(g.Remaining-w.Remaining) > 1e-9 || !g.ResetAt.Equal(w.ResetAt) ||
			g.Length != w.Length || !g.ObservedAt.Equal(w.ObservedAt) || g.Source != w.Source {
			t.Errorf("window %d:\n got  %+v\n want %+v", i, g, w)
		}
	}
}

func TestSnapshotAccessors(t *testing.T) {
	var empty Snapshot
	if !empty.Empty() || len(empty.Windows()) != 0 {
		t.Fatalf("zero Snapshot is not empty")
	}
	if _, ok := empty.First(FiveHour); ok {
		t.Fatalf("First on empty snapshot found a window")
	}

	s := Snapshot{windows: map[WindowID]Window{
		Weekly:   {ID: Weekly, Remaining: 0.5},
		FiveHour: {ID: FiveHour, Remaining: 0.9},
		Monthly:  {ID: Monthly, Remaining: 0.1},
	}}
	if s.Empty() {
		t.Fatalf("snapshot reported empty")
	}
	if w, ok := s.Window(Weekly); !ok || w.Remaining != 0.5 {
		t.Fatalf("Window(Weekly) = %+v, %v", w, ok)
	}
	if _, ok := s.Window(SevenDay); ok {
		t.Fatalf("Window(SevenDay) found a missing window")
	}
	if w, ok := s.First(SevenDay, Monthly, Weekly); !ok || w.ID != Monthly {
		t.Fatalf("First = %+v, %v; want monthly", w, ok)
	}
	var ids []WindowID
	for _, w := range s.Windows() {
		ids = append(ids, w.ID)
	}
	if want := []WindowID{FiveHour, Monthly, Weekly}; len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("Windows order = %v, want %v", ids, want)
	}
}

func TestParseProbeDispatch(t *testing.T) {
	claudeBody := []byte(`{"five_hour":{"utilization":10,"resets_at":null}}`)
	codexBody := []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`)
	tests := []struct {
		name     string
		provider string
		body     []byte
		wantErr  bool
		wantIDs  int
	}{
		{"claude mixed case", " Claude ", claudeBody, false, 1},
		{"codex upper case", "CODEX", codexBody, false, 1},
		{"unknown provider", "gemini", claudeBody, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseProbe(tt.provider, tt.body, testObservedAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != tt.wantIDs {
				t.Fatalf("got %d windows, want %d", len(got), tt.wantIDs)
			}
		})
	}
}
