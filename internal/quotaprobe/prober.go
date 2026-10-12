package quotaprobe

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// scheduleTick is how often the worker looks for accounts that are due.
	scheduleTick = 30 * time.Second
	// DefaultSpacing separates consecutive probes, so a refresh of every
	// account never bursts at the providers.
	DefaultSpacing  = 2 * time.Second
	maxErrorMessage = 300
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
	if entry := p.entries[authIndex]; entry != nil && p.now().Sub(entry.AttemptedAt) < p.minInterval {
		defer p.mu.Unlock()
		return *entry, nil
	}
	done, inflight := p.inflight[authIndex]
	if !inflight {
		done = make(chan struct{})
		p.inflight[authIndex] = done
	}
	// A requested probe goes ahead of scheduled ones; a running one is just awaited.
	if !inflight || p.dequeue(authIndex) {
		p.queue = append([]string{authIndex}, p.queue...)
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
		if auth == nil || auth.Disabled {
			continue
		}
		authIndex := strings.TrimSpace(auth.EnsureIndex())
		if authIndex == "" {
			continue
		}
		if _, ok := planFor(auth); !ok {
			continue
		}
		present[authIndex] = true
		if _, queued := p.inflight[authIndex]; queued {
			continue
		}
		if entry := p.entries[authIndex]; entry != nil && now.Sub(entry.AttemptedAt) < p.refreshInterval {
			continue
		}
		p.queue = append(p.queue, authIndex)
		p.inflight[authIndex] = make(chan struct{})
	}
	for authIndex := range p.entries {
		if !present[authIndex] {
			if _, queued := p.inflight[authIndex]; !queued {
				delete(p.entries, authIndex)
			}
		}
	}
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
	p.mu.Lock()
	entry := p.entries[authIndex]
	if entry == nil {
		entry = &Entry{AuthIndex: authIndex, Responses: map[string]Response{}}
		p.entries[authIndex] = entry
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
