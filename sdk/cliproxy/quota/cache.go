package quota

import (
	"sync"
	"time"
)

// View is what a selector reads. *Cache implements it.
type View interface {
	// Lookup merges the cached probe windows of authID with the windows parsed
	// from signals (observed at signalsAt). For each window ID the newer
	// ObservedAt wins; on a tie the probe wins. ok is false when no window is known.
	// Windows are returned as observed: a reset in the past is left for the caller.
	Lookup(authID, provider string, signals map[string]string, signalsAt time.Time) (Snapshot, bool)
	// RequestRefresh asks for a probe of authID soon. It never blocks.
	RequestRefresh(authID string)
}

// Cache holds the latest probe windows per account and merges them with the
// header signals on lookup. It is safe for concurrent use.
type Cache struct {
	mu sync.RWMutex
	// probes holds one slice per account. A stored slice is never mutated, so
	// readers may use it after releasing the lock.
	probes    map[string][]Window
	requester func(authID string)
}

var _ View = (*Cache)(nil)

// NewCache returns an empty cache without a refresh requester.
func NewCache() *Cache {
	return &Cache{probes: make(map[string][]Window)}
}

// PutProbe replaces the account's probe windows.
func (c *Cache) PutProbe(authID string, windows []Window) {
	stored := append([]Window(nil), windows...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(stored) == 0 {
		delete(c.probes, authID)
		return
	}
	if c.probes == nil {
		c.probes = make(map[string][]Window)
	}
	c.probes[authID] = stored
}

// Forget drops the account's probe windows.
func (c *Cache) Forget(authID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.probes, authID)
}

// SetRefreshRequester sets the function RequestRefresh calls. The function
// must not block. nil disables; RequestRefresh is then a no-op.
func (c *Cache) SetRefreshRequester(fn func(authID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requester = fn
}

// Lookup implements View.
func (c *Cache) Lookup(authID, provider string, signals map[string]string, signalsAt time.Time) (Snapshot, bool) {
	c.mu.RLock()
	probe := c.probes[authID]
	c.mu.RUnlock()

	headers := ParseHeaderSignals(provider, signals, signalsAt)
	if len(probe) == 0 && len(headers) == 0 {
		return Snapshot{}, false
	}
	merged := make(map[WindowID]Window, len(probe)+len(headers))
	for _, w := range probe {
		merged[w.ID] = w
	}
	for _, w := range headers {
		if current, ok := merged[w.ID]; ok && !w.ObservedAt.After(current.ObservedAt) {
			continue
		}
		merged[w.ID] = w
	}
	return Snapshot{windows: merged}, true
}

// RequestRefresh implements View by calling the refresh requester, if any.
func (c *Cache) RequestRefresh(authID string) {
	c.mu.RLock()
	fn := c.requester
	c.mu.RUnlock()
	if fn != nil {
		fn(authID)
	}
}
