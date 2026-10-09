package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// loopbackServer runs an optional auxiliary HTTP listener, such as pprof or
// metrics, whose enable flag and address can change on config reload. Each
// ApplyContext takes ownership through owner, so a stale apply never stops
// the server a newer one started.
type loopbackServer struct {
	name       string
	newHandler func() http.Handler

	mu      sync.Mutex
	server  *http.Server
	addr    string
	enabled bool
	owner   uint64
}

func newLoopbackServer(name string, newHandler func() http.Handler) *loopbackServer {
	return &loopbackServer{name: name, newHandler: newHandler}
}

// ApplyContext starts, restarts or stops the listener so that it matches
// enabled and addr. It reports false when ctx ended or a stop failed.
func (p *loopbackServer) ApplyContext(ctx context.Context, enabled bool, addr string) bool {
	if p == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	p.mu.Lock()
	p.owner++
	owner := p.owner
	currentServer := p.server
	currentAddr := p.addr
	p.addr = addr
	p.enabled = enabled
	if !enabled {
		p.server = nil
		p.mu.Unlock()
		if currentServer != nil {
			if errStop := p.stopServerWithContext(ctx, currentServer, currentAddr, "disabled"); errStop != nil {
				return false
			}
		}
		return ctx.Err() == nil
	}
	if currentServer != nil && currentAddr == addr {
		p.mu.Unlock()
		return ctx.Err() == nil
	}
	p.server = nil
	p.mu.Unlock()

	if currentServer != nil {
		if errStop := p.stopServerWithContext(ctx, currentServer, currentAddr, "restarted"); errStop != nil {
			return false
		}
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}

	startedServer := p.startServer(addr, owner)
	if errContext := ctx.Err(); errContext != nil {
		if startedServer != nil {
			go func() {
				_ = p.stopOwnedServerWithContext(context.Background(), startedServer, addr, "canceled", owner)
			}()
		}
		return false
	}
	return true
}

func (p *loopbackServer) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	currentServer := p.server
	currentAddr := p.addr
	p.owner++
	p.server = nil
	p.enabled = false
	p.mu.Unlock()

	if currentServer == nil {
		return nil
	}
	return p.stopServerWithContext(ctx, currentServer, currentAddr, "shutdown")
}

func (p *loopbackServer) startServer(addr string, owner uint64) *http.Server {
	server := &http.Server{
		Addr:              addr,
		Handler:           p.newHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	p.mu.Lock()
	if !p.enabled || p.addr != addr || p.owner != owner || p.server != nil {
		p.mu.Unlock()
		return nil
	}
	p.server = server
	p.mu.Unlock()

	log.Infof("%s server starting on %s", p.name, addr)
	go func() {
		if errServe := server.ListenAndServe(); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.Errorf("%s server failed on %s: %v", p.name, addr, errServe)
			p.clearFailedServer(server)
		}
	}()
	return server
}

// clearFailedServer removes a failed physical server even if a same-address
// ApplyContext transferred lifecycle ownership while ListenAndServe was starting.
func (p *loopbackServer) clearFailedServer(server *http.Server) {
	if p == nil || server == nil {
		return
	}
	p.mu.Lock()
	if p.server == server {
		p.server = nil
	}
	p.mu.Unlock()
}

func (p *loopbackServer) stopServer(server *http.Server, addr string, reason string) {
	_ = p.stopServerWithContext(context.Background(), server, addr, reason)
}

func (p *loopbackServer) stopOwnedServerWithContext(ctx context.Context, server *http.Server, addr string, reason string, owner uint64) error {
	if p == nil || server == nil {
		return nil
	}
	p.mu.Lock()
	if p.server != server || p.owner != owner {
		p.mu.Unlock()
		return nil
	}
	p.server = nil
	p.mu.Unlock()
	return p.stopServerWithContext(ctx, server, addr, reason)
}

func (p *loopbackServer) stopServerWithContext(ctx context.Context, server *http.Server, addr string, reason string) error {
	if server == nil {
		return nil
	}
	stopCtx := ctx
	if stopCtx == nil {
		stopCtx = context.Background()
	}
	stopCtx, cancel := context.WithTimeout(stopCtx, 5*time.Second)
	defer cancel()
	if errStop := server.Shutdown(stopCtx); errStop != nil {
		log.Errorf("%s server stop failed on %s: %v", p.name, addr, errStop)
		return errStop
	}
	log.Infof("%s server stopped on %s (%s)", p.name, addr, reason)
	return nil
}
