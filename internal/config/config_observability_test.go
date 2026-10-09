package config

import (
	"strings"
	"testing"
)

func TestUsageObservabilityV8PathsLoad(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`config-version: 8
observability:
  usage:
    store:
      enable: true
      path: " /var/lib/cliproxyapi/usage.db "
      retention-days: 7
  metrics:
    enable: true
    addr: "127.0.0.1:9999"
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	want := UsageStoreConfig{Enable: true, Path: "/var/lib/cliproxyapi/usage.db", RetentionDays: 7, RollupRetentionDays: DefaultUsageStoreRollupRetentionDays}
	if cfg.UsageStore != want {
		t.Fatalf("UsageStore = %+v, want %+v", cfg.UsageStore, want)
	}
	if cfg.Metrics != (MetricsConfig{Enable: true, Addr: "127.0.0.1:9999"}) {
		t.Fatalf("Metrics = %+v", cfg.Metrics)
	}
}

func TestUsageObservabilityDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.UsageStore.Enable || cfg.Metrics.Enable {
		t.Fatalf("store or metrics enabled by default: %+v %+v", cfg.UsageStore, cfg.Metrics)
	}
	if cfg.Metrics.Addr != DefaultMetricsAddr {
		t.Fatalf("Metrics.Addr = %q, want %q", cfg.Metrics.Addr, DefaultMetricsAddr)
	}
	if cfg.UsageStore.RetentionDays != DefaultUsageStoreRetentionDays || cfg.UsageStore.RollupRetentionDays != DefaultUsageStoreRollupRetentionDays {
		t.Fatalf("UsageStore retention = %+v", cfg.UsageStore)
	}
}

func TestUsageObservabilityRetention(t *testing.T) {
	for _, key := range []string{"retention-days", "rollup-retention-days"} {
		cfg, err := ParseConfigBytes([]byte("config-version: 8\nobservability:\n  usage:\n    store:\n      " + key + ": 0\n"))
		if err != nil {
			t.Fatalf("ParseConfigBytes(%s: 0) error = %v", key, err)
		}
		if cfg.UsageStore.RetentionDays != DefaultUsageStoreRetentionDays || cfg.UsageStore.RollupRetentionDays != DefaultUsageStoreRollupRetentionDays {
			t.Fatalf("%s: 0 gave %+v, want the defaults", key, cfg.UsageStore)
		}
		_, err = ParseConfigBytes([]byte("config-version: 8\nobservability:\n  usage:\n    store:\n      " + key + ": -1\n"))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("ParseConfigBytes(%s: -1) error = %v, want an error naming the key", key, err)
		}
	}
}
