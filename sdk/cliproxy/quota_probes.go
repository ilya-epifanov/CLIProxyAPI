package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaprobe"
	log "github.com/sirupsen/logrus"
)

// startQuotaProbes starts server-side quota reads when they are enabled. The
// prober stops with ctx.
func (s *Service) startQuotaProbes(ctx context.Context) {
	if s.cfg == nil || s.server == nil || s.coreManager == nil {
		return
	}
	s.quotaProbeConfig = s.cfg.QuotaProbe
	if !s.quotaProbeConfig.Enable {
		return
	}
	prober := s.server.NewQuotaProber(quotaprobe.Options{
		Auths:           s.coreManager.List,
		RefreshInterval: s.quotaProbeConfig.RefreshInterval,
		MinInterval:     s.quotaProbeConfig.MinRefreshInterval,
		Spacing:         quotaprobe.DefaultSpacing,
		Cache:           s.quotaCache,
		Cooldowns:       s.coreManager,
	})
	if s.quotaCache != nil {
		s.quotaCache.SetRefreshRequester(prober.RequestRefresh)
	}
	go prober.Run(ctx)
	log.Infof("quota probes started (every %s, at most every %s per account)",
		s.quotaProbeConfig.RefreshInterval, s.quotaProbeConfig.MinRefreshInterval)
}

// warnQuotaProbeRestart reports a reloaded quota probe setting, which applies
// only at the next start.
func (s *Service) warnQuotaProbeRestart(cfg *config.Config) {
	if s == nil || cfg == nil || s.server == nil || cfg.QuotaProbe == s.quotaProbeConfig {
		return
	}
	log.Warn("observability.quota changed; restart to apply")
}
