package quota

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCacheLookupMerge(t *testing.T) {
	t0 := testObservedAt
	reset := t0.Add(3 * time.Hour)
	probeWindows := []Window{
		{ID: FiveHour, Remaining: 0.2, ResetAt: reset, Length: 5 * time.Hour, ObservedAt: t0, Source: SourceProbe},
		{ID: SevenDay, Remaining: 0.6, ResetAt: reset, Length: 168 * time.Hour, ObservedAt: t0, Source: SourceProbe},
		{ID: SevenDayFable, Remaining: 0.3, ResetAt: reset, Length: 168 * time.Hour, ObservedAt: t0, Source: SourceProbe},
	}
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.5",
		"Anthropic-Ratelimit-Unified-5h-Reset":          fmt.Sprint(reset.Unix()),
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.9",
	}
	tests := []struct {
		name       string
		signalsAt  time.Time
		wantSource Source
		wantRemain float64
	}{
		{"probe newer wins", t0.Add(-time.Second), SourceProbe, 0.2},
		{"headers newer win", t0.Add(time.Second), SourceHeaders, 0.5},
		{"tie goes to probe", t0, SourceProbe, 0.2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCache()
			c.PutProbe("acc-1", probeWindows)
			snap, ok := c.Lookup("acc-1", "claude", signals, tt.signalsAt)
			if !ok {
				t.Fatalf("Lookup ok = false")
			}
			w, _ := snap.Window(FiveHour)
			if w.Source != tt.wantSource || w.Remaining != tt.wantRemain {
				t.Fatalf("five-hour = %+v, want source %s remaining %v", w, tt.wantSource, tt.wantRemain)
			}
			// Fable comes from the probe only, whatever the header timestamps.
			fable, ok := snap.Window(SevenDayFable)
			if !ok || fable.Source != SourceProbe || fable.Remaining != 0.3 {
				t.Fatalf("fable = %+v, %v", fable, ok)
			}
			if seven, _ := snap.Window(SevenDay); seven.Source != SourceProbe {
				t.Fatalf("seven-day source = %s, want probe", seven.Source)
			}
		})
	}
}

func TestCacheLookupHeadersOnlyAndEmpty(t *testing.T) {
	c := NewCache()
	now := testObservedAt
	if _, ok := c.Lookup("acc-1", "claude", nil, time.Time{}); ok {
		t.Fatalf("empty cache without signals: ok = true")
	}
	unrelated := map[string]string{"Retry-After": "30", "Anthropic-Ratelimit-Unified-Status": "allowed"}
	if _, ok := c.Lookup("acc-1", "claude", unrelated, now); ok {
		t.Fatalf("signals without windows: ok = true")
	}
	codex := map[string]string{"X-Codex-Primary-Used-Percent": "30", "X-Codex-Primary-Window-Minutes": "300"}
	snap, ok := c.Lookup("acc-1", "codex", codex, now)
	if !ok {
		t.Fatalf("headers-only lookup: ok = false")
	}
	if w, found := snap.First(Weekly, FiveHour); !found || w.ID != FiveHour || w.Source != SourceHeaders {
		t.Fatalf("headers-only window = %+v, %v", w, found)
	}
}

func TestCachePutProbeForget(t *testing.T) {
	c := NewCache()
	now := testObservedAt
	windows := []Window{{ID: FiveHour, Remaining: 0.5, Length: 5 * time.Hour, ObservedAt: now, Source: SourceProbe}}
	c.PutProbe("acc-1", windows)
	windows[0].Remaining = 0 // the cache keeps its own copy

	snap, ok := c.Lookup("acc-1", "codex", nil, time.Time{})
	if w, _ := snap.Window(FiveHour); !ok || w.Remaining != 0.5 {
		t.Fatalf("after PutProbe: %+v, %v", w, ok)
	}

	c.PutProbe("acc-1", []Window{{ID: Weekly, Remaining: 0.7, Length: 168 * time.Hour, ObservedAt: now, Source: SourceProbe}})
	snap, _ = c.Lookup("acc-1", "codex", nil, time.Time{})
	if _, found := snap.Window(FiveHour); found {
		t.Fatalf("PutProbe did not replace the previous windows")
	}

	c.Forget("acc-1")
	if _, ok := c.Lookup("acc-1", "codex", nil, time.Time{}); ok {
		t.Fatalf("after Forget: ok = true")
	}
}

func TestCacheRequestRefresh(t *testing.T) {
	c := NewCache()
	c.RequestRefresh("acc-1") // no requester: no-op

	var got []string
	c.SetRefreshRequester(func(authID string) { got = append(got, authID) })
	c.RequestRefresh("acc-1")
	c.RequestRefresh("acc-2")
	if len(got) != 2 || got[0] != "acc-1" || got[1] != "acc-2" {
		t.Fatalf("requested = %v", got)
	}

	c.SetRefreshRequester(nil)
	c.RequestRefresh("acc-3")
	if len(got) != 2 {
		t.Fatalf("requester called after being disabled: %v", got)
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := NewCache()
	now := testObservedAt
	signals := map[string]string{"X-Codex-Primary-Used-Percent": "30", "X-Codex-Primary-Window-Minutes": "300"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		authID := fmt.Sprintf("acc-%d", i%2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.PutProbe(authID, []Window{{ID: Weekly, Remaining: float64(j) / 200, Length: 168 * time.Hour, ObservedAt: now, Source: SourceProbe}})
				if j%50 == 0 {
					c.Forget(authID)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if _, ok := c.Lookup(authID, "codex", signals, now); !ok {
					t.Errorf("Lookup with header signals: ok = false")
					return
				}
				c.RequestRefresh(authID)
			}
		}()
	}
	wg.Wait()
}
