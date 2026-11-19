package quotaprobe

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

const (
	// scheduleTick is how often the worker looks for accounts that are due.
	scheduleTick = 30 * time.Second
	// DefaultSpacing separates consecutive probes, so a refresh of every
	// account never bursts at the providers.
	DefaultSpacing  = 2 * time.Second
	maxErrorMessage = 300
	// refreshRequests bounds pending RequestRefresh calls; more are dropped.
	refreshRequests = 64
)

var (
	// ErrUnknownAuth means no credential has the requested auth_index.
	ErrUnknownAuth = errors.New("quota probe: credential not found")
	// ErrUnsupportedProvider means the credential's provider is not probed.
	ErrUnsupportedProvider = errors.New("quota probe: provider not supported")
)

// Response is one upstream answer, kept as received.
type Response struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// Doer sends one GET with the account's credentials in its Authorization header.
type Doer interface {
	DoQuotaProbe(ctx context.Context, auth *coreauth.Auth, request Request) (Response, error)
}

// Failure describes the latest probe when it failed.
type Failure struct {
	// Status is the upstream HTTP status, or 0 when the request did not complete.
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
}

// Entry is the probe state of one account. A probe replaces its FetchedAt,
// Error and Responses instead of editing them, so a copy of an Entry value
// stays consistent.
type Entry struct {
	AuthIndex string `json:"auth_index"`
	// AttemptedAt is the latest probe, successful or not.
	AttemptedAt time.Time `json:"attempted_at"`
	// FetchedAt is the latest successful probe; nil before the first one.
	FetchedAt *time.Time `json:"fetched_at"`
	// Error is set when the latest probe failed; Responses then still hold the
	// last successful probe.
	Error     *Failure            `json:"error"`
	Responses map[string]Response `json:"responses"`
	// authID is the auth ID the quota cache is keyed by; it is not serialized.
	authID string
}

// CooldownReleaser clears a quota cooldown that a read shows is over;
// *coreauth.Manager implements it.
type CooldownReleaser interface {
	ReleaseRecoveredQuotaCooldown(ctx context.Context, authID string, windows []quota.Window, sentAt time.Time) (bool, error)
}

// Options configures New.
type Options struct {
	Doer  Doer
	Auths func() []*coreauth.Auth
	// RefreshInterval is how often each account is probed on schedule.
	RefreshInterval time.Duration
	// MinInterval is the shortest time between two probes of one account,
	// scheduled or requested.
	MinInterval time.Duration
	// Spacing separates consecutive probes of different accounts.
	Spacing time.Duration
	Now     func() time.Time
	// Cache receives the windows parsed from each successful probe; optional.
	Cache *quota.Cache
	// Cooldowns receives the same windows, so a reset made outside the proxy
	// ends the account's quota cooldown; optional.
	Cooldowns CooldownReleaser
}

// Prober probes accounts one at a time from a single queue.
type Prober struct {
	doer            Doer
	auths           func() []*coreauth.Auth
	refreshInterval time.Duration
	minInterval     time.Duration
	spacing         time.Duration
	now             func() time.Time
	wake            chan struct{}
	cache           *quota.Cache
	cooldowns       CooldownReleaser
	// requests carries auth IDs from RequestRefresh to Run.
	requests chan string

	mu      sync.Mutex
	entries map[string]*Entry
	queue   []string
	// inflight holds every queued or running probe; the probe closes its
	// channel when it is recorded.
	inflight map[string]chan struct{}
}

// New returns a prober; Run starts it.
func New(opts Options) *Prober {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Prober{
		doer:            opts.Doer,
		auths:           opts.Auths,
		refreshInterval: opts.RefreshInterval,
		minInterval:     opts.MinInterval,
		spacing:         opts.Spacing,
		now:             now,
		wake:            make(chan struct{}, 1),
		cache:           opts.Cache,
		cooldowns:       opts.Cooldowns,
		requests:        make(chan string, refreshRequests),
		entries:         map[string]*Entry{},
		inflight:        map[string]chan struct{}{},
	}
}

// Run probes due and requested accounts until ctx ends.
func (p *Prober) Run(ctx context.Context) {
	ticker := time.NewTicker(scheduleTick)
	defer ticker.Stop()
	for {
		p.enqueueDue()
		p.enqueueRequested()
		if authIndex, ok := p.next(); ok {
			p.probe(ctx, authIndex)
			if !sleep(ctx, p.spacing) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Refresh probes the account now unless it was probed within MinInterval, and
// returns its entry. A probe already queued for the account is shared.
func (p *Prober) Refresh(ctx context.Context, authIndex string) (Entry, error) {
	authIndex = strings.TrimSpace(authIndex)
	auth := p.findAuth(authIndex)
	if auth == nil {
		return Entry{}, ErrUnknownAuth
	}
	if _, ok := planFor(auth); !ok {
		return Entry{}, ErrUnsupportedProvider
	}

	p.mu.Lock()
	if p.attemptedWithinLocked(authIndex, p.minInterval, p.now()) {
		defer p.mu.Unlock()
		return *p.entries[authIndex], nil
	}
	done, inflight := p.inflight[authIndex]
	// A requested probe goes ahead of scheduled ones; a running one is just awaited.
	if !inflight {
		done = p.enqueueLocked(authIndex, true)
	} else if p.dequeue(authIndex) {
		p.enqueueLocked(authIndex, true)
	}
	p.mu.Unlock()
	p.signal()

	select {
	case <-done:
	case <-ctx.Done():
		return Entry{}, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.entries[authIndex], nil
}

// Entries returns the probe state of every probed account, ordered by
// auth_index. Accounts that no longer exist drop out at the next schedule check.
func (p *Prober) Entries() []Entry {
	p.mu.Lock()
	out := make([]Entry, 0, len(p.entries))
	for _, entry := range p.entries {
		out = append(out, *entry)
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out
}

// RequestRefresh asks for a probe of the account with auth ID authID soon, unless it
// was probed within MinInterval or is already queued. It never blocks: a request that
// does not fit the buffer is dropped, and the schedule still probes the account later.
func (p *Prober) RequestRefresh(authID string) {
	select {
	case p.requests <- authID:
		p.signal()
	default:
	}
}

// enqueueRequested queues the accounts asked for by RequestRefresh.
func (p *Prober) enqueueRequested() {
	ids := map[string]bool{}
drain:
	for {
		select {
		case authID := <-p.requests:
			ids[authID] = true
		default:
			break drain
		}
	}
	if len(ids) == 0 {
		return
	}
	auths := p.auths()
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, auth := range auths {
		if auth == nil || !ids[auth.ID] {
			continue
		}
		authIndex, probed := probedIndex(auth)
		if !probed || !p.dueLocked(authIndex, p.minInterval, now) {
			continue
		}
		p.enqueueLocked(authIndex, true)
	}
}

// probedIndex returns the auth_index of auth when it is an enabled account of a
// probed provider.
func probedIndex(auth *coreauth.Auth) (string, bool) {
	if auth == nil || auth.Disabled {
		return "", false
	}
	authIndex := strings.TrimSpace(auth.EnsureIndex())
	if authIndex == "" {
		return "", false
	}
	_, ok := planFor(auth)
	return authIndex, ok
}

// attemptedWithinLocked reports whether the account was probed less than interval
// before now. The caller holds mu.
func (p *Prober) attemptedWithinLocked(authIndex string, interval time.Duration, now time.Time) bool {
	entry := p.entries[authIndex]
	return entry != nil && now.Sub(entry.AttemptedAt) < interval
}

// dueLocked reports whether the account may be queued: it is not queued or running,
// and was not probed within interval. The caller holds mu.
func (p *Prober) dueLocked(authIndex string, interval time.Duration, now time.Time) bool {
	_, queued := p.inflight[authIndex]
	return !queued && !p.attemptedWithinLocked(authIndex, interval, now)
}

func (p *Prober) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// enqueueDue queues every enabled, probed account whose latest probe is at
// least RefreshInterval old, and forgets accounts that no longer exist.
func (p *Prober) enqueueDue() {
	auths := p.auths()
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	present := map[string]bool{}
	for _, auth := range auths {
		authIndex, probed := probedIndex(auth)
		if !probed {
			continue
		}
		present[authIndex] = true
		if !p.dueLocked(authIndex, p.refreshInterval, now) {
			continue
		}
		p.enqueueLocked(authIndex, false)
	}
	for authIndex := range p.entries {
		if !present[authIndex] {
			if _, queued := p.inflight[authIndex]; !queued {
				entry := p.entries[authIndex]
				delete(p.entries, authIndex)
				if p.cache != nil {
					p.cache.Forget(entry.authID)
				}
			}
		}
	}
}

// enqueueLocked queues authIndex at the front or the back and returns its inflight
// channel, creating it when missing: every queued index has one. The caller holds mu.
func (p *Prober) enqueueLocked(authIndex string, front bool) chan struct{} {
	done, ok := p.inflight[authIndex]
	if !ok {
		done = make(chan struct{})
		p.inflight[authIndex] = done
	}
	if front {
		p.queue = append([]string{authIndex}, p.queue...)
	} else {
		p.queue = append(p.queue, authIndex)
	}
	return done
}

// dequeue removes authIndex from the queue and reports whether it was there.
// The caller holds mu.
func (p *Prober) dequeue(authIndex string) bool {
	for i, queued := range p.queue {
		if queued == authIndex {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return true
		}
	}
	return false
}

func (p *Prober) next() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return "", false
	}
	authIndex := p.queue[0]
	p.queue = p.queue[1:]
	return authIndex, true
}

func (p *Prober) findAuth(authIndex string) *coreauth.Auth {
	if authIndex == "" {
		return nil
	}
	for _, auth := range p.auths() {
		if auth != nil && strings.TrimSpace(auth.EnsureIndex()) == authIndex {
			return auth
		}
	}
	return nil
}

// probe sends the account's requests in order and records the outcome. When
// the primary (first) request fails, the rest are not sent.
func (p *Prober) probe(ctx context.Context, authIndex string) {
	auth := p.findAuth(authIndex)
	var plan []Request
	if auth != nil {
		plan, _ = planFor(auth)
	}

	responses := map[string]Response{}
	var failure *Failure
	sentAt := p.now()
	if len(plan) == 0 {
		failure = &Failure{Message: "credential not found or not probed"}
	}
	for i, request := range plan {
		response, err := p.doer.DoQuotaProbe(ctx, auth, request)
		primary := i == 0
		switch {
		case err != nil && primary:
			failure = &Failure{Message: err.Error()}
		case err != nil:
			log.WithError(err).WithField("auth_index", authIndex).Debugf("quota probe: %s request failed", request.Name)
			continue
		case primary && (response.StatusCode < 200 || response.StatusCode >= 300):
			failure = &Failure{Status: response.StatusCode, Message: truncate(strings.TrimSpace(response.Body), maxErrorMessage)}
		}
		if failure != nil {
			break
		}
		responses[request.Name] = response
	}

	now := p.now()
	if failure == nil {
		p.storeWindows(ctx, auth, responses[plan[0].Name], sentAt, now)
	}
	p.mu.Lock()
	entry := p.entries[authIndex]
	if entry == nil {
		entry = &Entry{AuthIndex: authIndex, Responses: map[string]Response{}}
		p.entries[authIndex] = entry
	}
	if auth != nil {
		entry.authID = auth.ID
	}
	entry.AttemptedAt = now
	entry.Error = failure
	if failure == nil {
		fetchedAt := now
		entry.FetchedAt = &fetchedAt
		entry.Responses = responses
	}
	if done, ok := p.inflight[authIndex]; ok {
		close(done)
		delete(p.inflight, authIndex)
	}
	p.mu.Unlock()

	if failure != nil {
		log.WithFields(log.Fields{"auth_index": authIndex, "status": failure.Status}).Warnf("quota probe failed: %s", failure.Message)
	}
}

// storeWindows parses the primary response into the cache and lets the cooldown
// releaser end a quota cooldown it shows is over. A body the parser rejects keeps
// the previous windows, which then age into stale.
func (p *Prober) storeWindows(ctx context.Context, auth *coreauth.Auth, response Response, sentAt, observedAt time.Time) {
	if p.cache == nil && p.cooldowns == nil {
		return
	}
	windows, err := quota.ParseProbe(auth.Provider, []byte(response.Body), observedAt)
	if err != nil {
		log.WithError(err).WithField("auth_id", auth.ID).Warn("quota probe: cannot parse usage response")
		return
	}
	if p.cache != nil {
		p.cache.PutProbe(auth.ID, windows)
	}
	if p.cooldowns != nil {
		if _, errRelease := p.cooldowns.ReleaseRecoveredQuotaCooldown(ctx, auth.ID, windows, sentAt); errRelease != nil {
			log.WithError(errRelease).WithField("auth_id", auth.ID).Warn("quota probe: cannot clear recovered cooldown")
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
