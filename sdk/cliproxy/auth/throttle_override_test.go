package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThrottleOverridesSurviveALoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "throttle-overrides.json")
	overrides, err := LoadThrottleOverrides(path)
	if err != nil {
		t.Fatalf("load without a file: %v", err)
	}
	if _, err := overrides.Set("claude", 1, paceNow); err != nil {
		t.Fatal(err)
	}
	if _, err := overrides.Set("codex", 0.4, paceNow); err != nil {
		t.Fatal(err)
	}
	if err := overrides.Clear("codex"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadThrottleOverrides(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, ok := loaded.Get("claude"); !ok || got.Speed != 1 || !got.SetAt.Equal(paceNow) {
		t.Fatalf("claude = %+v, %v; want speed 1 set at %s", got, ok, paceNow)
	}
	if _, ok := loaded.Get("codex"); ok {
		t.Fatal("codex override survived its clear")
	}
}

func TestLoadThrottleOverridesRejectsABadFile(t *testing.T) {
	for name, content := range map[string]string{
		"speed":   `{"version":1,"overrides":{"claude":{"speed":1.5}}}`,
		"version": `{"version":2,"overrides":{}}`,
		"json":    `{`,
	} {
		path := filepath.Join(t.TempDir(), "throttle-overrides.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadThrottleOverrides(path); err == nil {
			t.Errorf("%s: load succeeded", name)
		}
	}
}

func TestOverrideRunsAShortPoolAtFullSpeed(t *testing.T) {
	manager, recorder := newThrottledManager(t, throttleOn, shortScenario)
	if _, err := manager.ThrottleOverrides().Set("claude", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, manager); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(recorder.waits) != 0 {
		t.Fatalf("waits = %v; want none", recorder.waits)
	}
	r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway
	if r == nil || r.Override == nil || r.Reason != RunwayShort || r.Delay != 0 || r.ComputedDelay <= 0 {
		t.Fatalf("runway = %+v; want the computed short runway, no delay, a computed delay", r)
	}
}

func TestOverrideSlowsAPoolThePlanWouldNot(t *testing.T) {
	learning := shortScenario
	learning.sevenDay, learning.fiveHour = 0.95, 0.95
	for name, sc := range map[string]throttleScenario{"clear": clearScenario, "learning": learning} {
		manager, recorder := newThrottledManager(t, throttleOn, sc)
		if _, err := manager.ThrottleOverrides().Set("claude", 0.5, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := execute(t, manager); err != nil {
			t.Fatalf("%s: Execute: %v", name, err)
		}
		// 10 s turns at speed 0.5 wait 10 s.
		if len(recorder.waits) != 1 || (recorder.waits[0]-10*time.Second).Abs() > time.Second {
			t.Fatalf("%s: waits = %v; want one of about 10s", name, recorder.waits)
		}
		if err := manager.ThrottleOverrides().Clear("claude"); err != nil {
			t.Fatal(err)
		}
		if r := manager.RankCandidates(context.Background(), "claude", "claude-opus-5-5").Runway; r == nil || r.Override != nil || r.Delay != 0 {
			t.Fatalf("%s: cleared runway = %+v; want no override and no delay", name, r)
		}
	}
}
