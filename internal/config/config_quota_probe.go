package config

import "fmt"

func (cfg *Config) setQuotaProbeDefaults() {
	cfg.QuotaProbe.Enable = false
	cfg.QuotaProbe.RefreshInterval = DefaultQuotaProbeRefreshInterval
	cfg.QuotaProbe.MinRefreshInterval = DefaultQuotaProbeMinRefreshInterval
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
