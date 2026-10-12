package config

import "time"

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultMetricsAddr           = "127.0.0.1:9464"

	DefaultQuotaProbeRefreshInterval    = 15 * time.Minute
	DefaultQuotaProbeMinRefreshInterval = 5 * time.Minute

	DefaultUsageStoreRetentionDays       = 30
	DefaultUsageStoreRollupRetentionDays = 730
	DefaultAuthDir                       = "~/.cli-proxy-api"
	DefaultDiscoveryServiceType          = "_ai-gateway._tcp"
)
