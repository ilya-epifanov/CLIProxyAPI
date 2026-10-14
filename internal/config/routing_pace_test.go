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
