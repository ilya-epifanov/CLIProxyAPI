package config

import (
	"fmt"
	"strings"
)

func (cfg *Config) setUsageObservabilityDefaults() {
	cfg.Metrics.Enable = false
	cfg.Metrics.Addr = DefaultMetricsAddr
	cfg.UsageStore.Enable = false
	cfg.UsageStore.RetentionDays = DefaultUsageStoreRetentionDays
	cfg.UsageStore.RollupRetentionDays = DefaultUsageStoreRollupRetentionDays
	cfg.QuotaProbe.Enable = false
	cfg.QuotaProbe.RefreshInterval = DefaultQuotaProbeRefreshInterval
	cfg.QuotaProbe.MinRefreshInterval = DefaultQuotaProbeMinRefreshInterval
}

// normalizeUsageObservability trims the metrics and usage store settings.
// A retention of 0 means the default, as elsewhere in the config: saving the
// config drops zero values. A negative retention is rejected.
func (cfg *Config) normalizeUsageObservability() error {
	cfg.Metrics.Addr = strings.TrimSpace(cfg.Metrics.Addr)
	if cfg.Metrics.Addr == "" {
		cfg.Metrics.Addr = DefaultMetricsAddr
	}
	cfg.UsageStore.Path = strings.TrimSpace(cfg.UsageStore.Path)
	if cfg.UsageStore.RetentionDays < 0 {
		return fmt.Errorf("observability.usage.store.retention-days must not be negative, got %d", cfg.UsageStore.RetentionDays)
	}
	if cfg.UsageStore.RetentionDays == 0 {
		cfg.UsageStore.RetentionDays = DefaultUsageStoreRetentionDays
	}
	if cfg.UsageStore.RollupRetentionDays < 0 {
		return fmt.Errorf("observability.usage.store.rollup-retention-days must not be negative, got %d", cfg.UsageStore.RollupRetentionDays)
	}
	if cfg.UsageStore.RollupRetentionDays == 0 {
		cfg.UsageStore.RollupRetentionDays = DefaultUsageStoreRollupRetentionDays
	}
	return cfg.normalizeQuotaProbe()
}

// normalizeQuotaProbe applies the defaults for zero intervals and rejects a
// schedule that would read an account more often than the minimum allows.
func (cfg *Config) normalizeQuotaProbe() error {
	probe := &cfg.QuotaProbe
	if probe.RefreshInterval < 0 || probe.MinRefreshInterval < 0 {
		return fmt.Errorf("observability.quota intervals must not be negative")
	}
	if probe.RefreshInterval == 0 {
		probe.RefreshInterval = DefaultQuotaProbeRefreshInterval
	}
	if probe.MinRefreshInterval == 0 {
		probe.MinRefreshInterval = DefaultQuotaProbeMinRefreshInterval
	}
	if probe.RefreshInterval < probe.MinRefreshInterval {
		return fmt.Errorf("observability.quota.refresh-interval (%s) must not be shorter than min-refresh-interval (%s)",
			probe.RefreshInterval, probe.MinRefreshInterval)
	}
	return nil
}
