package config

import (
	"fmt"
	"time"
)

const (
	DefaultQuotaProbeRefreshInterval    = 15 * time.Minute
	DefaultQuotaProbeMinRefreshInterval = 5 * time.Minute
)

// normalizeQuotaProbe is the only place the quota-probe defaults are applied:
// it fills zero intervals and rejects a schedule that would read an account
// more often than the minimum allows.
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
