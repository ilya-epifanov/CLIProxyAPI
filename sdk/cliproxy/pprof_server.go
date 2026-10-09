package cliproxy

import (
	"context"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type pprofServer struct {
	*loopbackServer
}

func newPprofServer() *pprofServer {
	return &pprofServer{newLoopbackServer("pprof", func() http.Handler { return newPprofMux() })}
}

func (s *Service) applyPprofConfig(cfg *config.Config) {
	s.applyPprofConfigContext(context.Background(), cfg)
}

func (s *Service) applyPprofConfigContext(ctx context.Context, cfg *config.Config) bool {
	if s == nil || cfg == nil || (ctx != nil && ctx.Err() != nil) {
		return false
	}
	if s.applyPprofConfigContextFn != nil {
		return s.applyPprofConfigContextFn(ctx, cfg)
	}
	if s.pprofServer == nil {
		s.pprofServer = newPprofServer()
	}
	return s.pprofServer.ApplyContext(ctx, cfg)
}

func (s *Service) shutdownPprof(ctx context.Context) error {
	if s == nil || s.pprofServer == nil {
		return nil
	}
	return s.pprofServer.Shutdown(ctx)
}

func (p *pprofServer) Apply(cfg *config.Config) {
	p.ApplyContext(context.Background(), cfg)
}

func (p *pprofServer) ApplyContext(ctx context.Context, cfg *config.Config) bool {
	if p == nil || cfg == nil {
		return false
	}
	addr := strings.TrimSpace(cfg.Pprof.Addr)
	if addr == "" {
		addr = config.DefaultPprofAddr
	}
	return p.loopbackServer.ApplyContext(ctx, cfg.Pprof.Enable, addr)
}

func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	return mux
}
