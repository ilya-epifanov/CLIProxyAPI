package cliproxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// metricsServer serves /metrics on the address in observability.metrics.
type metricsServer struct {
	*loopbackServer
}

func newMetricsServer(handler http.Handler) *metricsServer {
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	return &metricsServer{newLoopbackServer("metrics", func() http.Handler { return mux })}
}

func (m *metricsServer) ApplyContext(ctx context.Context, cfg *config.Config) bool {
	if m == nil || cfg == nil {
		return false
	}
	addr := strings.TrimSpace(cfg.Metrics.Addr)
	if addr == "" {
		addr = config.DefaultMetricsAddr
	}
	return m.loopbackServer.ApplyContext(ctx, cfg.Metrics.Enable, addr)
}

// applyMetricsConfigContext is a no-op until Run has built the metrics server.
func (s *Service) applyMetricsConfigContext(ctx context.Context, cfg *config.Config) bool {
	if s == nil || cfg == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if s.metricsServer == nil {
		return true
	}
	return s.metricsServer.ApplyContext(ctx, cfg)
}

func (s *Service) shutdownMetrics(ctx context.Context) error {
	if s == nil || s.metricsServer == nil {
		return nil
	}
	return s.metricsServer.Shutdown(ctx)
}
