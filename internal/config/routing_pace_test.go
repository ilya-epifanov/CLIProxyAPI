package config

import (
	"strings"
	"testing"
	"time"
)

func TestPaceRoutingConfigLoads(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
routing:
  strategy: pace
  pace:
    burst-reserve: 0
    pace-tolerance: 0.25
    stale-after: 5m
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	pace := cfg.Routing.Pace
	if pace.BurstReserveOrDefault() != 0 || pace.PaceToleranceOrDefault() != 0.25 || pace.StaleAfterOrDefault() != 5*time.Minute {
		t.Fatalf("pace = %+v", pace)
	}
}

func TestPaceRoutingConfigDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  strategy: pace\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	pace := cfg.Routing.Pace
	if pace.BurstReserveOrDefault() != DefaultPaceBurstReserve || pace.PaceToleranceOrDefault() != DefaultPaceTolerance || pace.StaleAfterOrDefault() != DefaultPaceStaleAfter {
		t.Fatalf("defaults = %v %v %v", pace.BurstReserveOrDefault(), pace.PaceToleranceOrDefault(), pace.StaleAfterOrDefault())
	}
}

func TestPaceRoutingConfigRejectsOutOfRange(t *testing.T) {
	for _, tc := range []struct{ yaml, field string }{
		{"burst-reserve: 1.5", "burst-reserve"},
		{"burst-reserve: -0.1", "burst-reserve"},
		{"pace-tolerance: 1", "pace-tolerance"},
		{"stale-after: -1m", "stale-after"},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  pace:\n    " + tc.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), "routing.pace."+tc.field) {
				t.Fatalf("error = %v, want one naming routing.pace.%s", err, tc.field)
			}
		})
	}
}

func TestPaceThrottleConfigLoadsAndDefaultsOff(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
routing:
  strategy: pace
  pace:
    throttle:
      margin: 0.3
      guard-horizon: 0s
      max-delay: 90s
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	throttle := cfg.Routing.Pace.Throttle
	if throttle.MarginOrDefault() != 0.3 || throttle.GuardHorizonOrDefault() != 0 || throttle.MaxDelay != 90*time.Second {
		t.Fatalf("throttle = %+v", throttle)
	}

	defaults, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  strategy: pace\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	off := defaults.Routing.Pace.Throttle
	if off.MaxDelay != 0 || off.MarginOrDefault() != DefaultThrottleMargin || off.GuardHorizonOrDefault() != DefaultThrottleGuard {
		t.Fatalf("default throttle = %+v", off)
	}
}

func TestPaceThrottleConfigRejectsOutOfRange(t *testing.T) {
	for _, tc := range []struct{ yaml, field string }{
		{"margin: -0.1", "margin"},
		{"margin: 1.5", "margin"},
		{"guard-horizon: -1h", "guard-horizon"},
		{"max-delay: -1s", "max-delay"},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  pace:\n    throttle:\n      " + tc.yaml + "\n"))
			if err == nil || !strings.Contains(err.Error(), "routing.pace.throttle."+tc.field) {
				t.Fatalf("error = %v, want one naming routing.pace.throttle.%s", err, tc.field)
			}
		})
	}
}

func TestPaceThrottleConfigWarnsNearClientTimeouts(t *testing.T) {
	if warning := (PaceThrottleConfig{MaxDelay: 120 * time.Second}).Warning(); warning != "" {
		t.Fatalf("120s warned: %q", warning)
	}
	if warning := (PaceThrottleConfig{MaxDelay: 240 * time.Second}).Warning(); !strings.Contains(warning, "max-delay") {
		t.Fatalf("240s warning = %q", warning)
	}
}
