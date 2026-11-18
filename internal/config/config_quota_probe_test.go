package config

import (
	"testing"
	"time"
)

func TestQuotaProbeConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nobservability:\n  quota:\n    enable: true\n    refresh-interval: 30m\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	want := QuotaProbeConfig{Enable: true, RefreshInterval: 30 * time.Minute, MinRefreshInterval: DefaultQuotaProbeMinRefreshInterval}
	if cfg.QuotaProbe != want {
		t.Fatalf("QuotaProbe = %+v, want %+v", cfg.QuotaProbe, want)
	}

	cfg, err = ParseConfigBytes([]byte("config-version: 8\n"))
	if err != nil || cfg.QuotaProbe != (QuotaProbeConfig{RefreshInterval: 15 * time.Minute, MinRefreshInterval: 5 * time.Minute}) {
		t.Fatalf("defaults = %+v, %v", cfg.QuotaProbe, err)
	}

	for _, body := range []string{
		"refresh-interval: 2m",        // shorter than the 5m minimum
		"min-refresh-interval: -1m",   // negative
		"refresh-interval: sometimes", // not a duration
	} {
		if _, err = ParseConfigBytes([]byte("config-version: 8\nobservability:\n  quota:\n    " + body + "\n")); err == nil {
			t.Fatalf("ParseConfigBytes accepted %q", body)
		}
	}
}
