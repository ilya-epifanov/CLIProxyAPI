package quotaprobe

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeDoer struct {
	mu        sync.Mutex
	calls     []string // "<auth_index> <request name>"
	responses map[string]Response
	errs      map[string]error
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (f *fakeDoer) DoQuotaProbe(_ context.Context, auth *coreauth.Auth, request Request) (Response, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		if m := f.maxFlight.Load(); n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, auth.Index+" "+request.Name)
	if err := f.errs[request.Name]; err != nil {
		return Response{}, err
	}
	if response, ok := f.responses[request.Name]; ok {
		return response, nil
	}
	return Response{StatusCode: http.StatusOK, Body: `{"from":"` + request.Name + `"}`}, nil
}

func (f *fakeDoer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestProber(doer *fakeDoer, auths ...*coreauth.Auth) (*Prober, *clock) {
	c := &clock{now: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)}
	return New(Options{
		Doer: doer,
		// Clones, as coreauth.Manager.List returns: EnsureIndex writes to the auth.
		Auths: func() []*coreauth.Auth {
			out := make([]*coreauth.Auth, len(auths))
			for i, auth := range auths {
				out[i] = auth.Clone()
			}
			return out
		},
		RefreshInterval: 15 * time.Minute,
		MinInterval:     5 * time.Minute,
		Now:             c.Now,
	}), c
}

func claudeAuth(index string) *coreauth.Auth {
	return &coreauth.Auth{Index: index, Provider: "claude"}
}

// drain runs queued probes the way Run does, without its timers.
func drain(p *Prober) {
	p.enqueueDue()
	for {
		authIndex, ok := p.next()
		if !ok {
			return
		}
		p.probe(context.Background(), authIndex)
	}
}

func TestScheduleProbesEachEnabledAccountOncePerInterval(t *testing.T) {
	doer := &fakeDoer{}
	disabled := claudeAuth("off")
	disabled.Disabled = true
	p, c := newTestProber(doer, claudeAuth("a1"), disabled, &coreauth.Auth{Index: "x1", Provider: "gemini"})

	drain(p)
	if got := doer.callCount(); got != 3 {
		t.Fatalf("first sweep sent %d requests, want the 3 Claude requests of a1: %v", got, doer.calls)
	}
	c.Advance(14 * time.Minute)
	drain(p)
	if got := doer.callCount(); got != 3 {
		t.Fatalf("a probe 14 minutes later sent requests: %v", doer.calls)
	}
	c.Advance(time.Minute)
	drain(p)
	if got := doer.callCount(); got != 6 {
		t.Fatalf("a probe after 15 minutes sent %d requests in total, want 6", got)
	}
}

func TestRefreshWithinMinIntervalReturnsTheCachedEntry(t *testing.T) {
	doer := &fakeDoer{}
	p, c := newTestProber(doer, claudeAuth("a1"))
	drain(p)
	fetchedAt := c.Now()

	c.Advance(4*time.Minute + 59*time.Second)
	entry, err := p.Refresh(context.Background(), "a1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if doer.callCount() != 3 {
		t.Fatalf("Refresh within the minimum interval sent requests: %v", doer.calls)
	}
	if entry.FetchedAt == nil || !entry.FetchedAt.Equal(fetchedAt) || entry.Responses["usage"].Body != `{"from":"usage"}` {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestRefreshAfterMinIntervalProbesAndSharesOneProbe(t *testing.T) {
	doer := &fakeDoer{}
	p, c := newTestProber(doer, claudeAuth("a1"))
	drain(p)
	c.Advance(5 * time.Minute)

	results := make(chan Entry, 3)
	for i := 0; i < 3; i++ {
		go func() {
			entry, err := p.Refresh(context.Background(), "a1")
			if err != nil {
				t.Errorf("Refresh: %v", err)
			}
			results <- entry
		}()
	}
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.queue) == 1
	})
	// A refresh that arrives after this probe is inside the minimum interval,
	// so whatever the timing, all three get this probe.
	authIndex, _ := p.next()
	p.probe(context.Background(), authIndex)
	for i := 0; i < 3; i++ {
		if entry := <-results; entry.FetchedAt == nil || !entry.FetchedAt.Equal(c.Now()) {
			t.Fatalf("Refresh returned %+v, want the new probe", entry)
		}
	}
	if got := doer.callCount(); got != 6 {
		t.Fatalf("three refreshes sent %d requests in total, want one probe (3 more)", got)
	}
}

func TestPrimaryFailureKeepsTheLastGoodResponses(t *testing.T) {
	doer := &fakeDoer{}
	p, c := newTestProber(doer, claudeAuth("a1"))
	drain(p)
	firstFetch := c.Now()

	c.Advance(15 * time.Minute)
	doer.responses = map[string]Response{"usage": {StatusCode: http.StatusTooManyRequests, Body: `{"error":"rate limited"}`}}
	drain(p)

	entry := p.Entries()[0]
	if entry.Error == nil || entry.Error.Status != http.StatusTooManyRequests {
		t.Fatalf("Error = %+v, want the 429", entry.Error)
	}
	if !entry.AttemptedAt.Equal(c.Now()) || entry.FetchedAt == nil || !entry.FetchedAt.Equal(firstFetch) {
		t.Fatalf("attempted %v fetched %v", entry.AttemptedAt, entry.FetchedAt)
	}
	if entry.Responses["usage"].StatusCode != http.StatusOK || len(entry.Responses) != 3 {
		t.Fatalf("responses = %+v, want the first probe's", entry.Responses)
	}
	if got := doer.callCount(); got != 4 {
		t.Fatalf("a failed primary request sent %d requests in total, want 4 (the rest skipped)", got)
	}

	// The failed attempt also starts the minimum interval.
	c.Advance(time.Minute)
	if _, err := p.Refresh(context.Background(), "a1"); err != nil || doer.callCount() != 4 {
		t.Fatalf("Refresh right after a failure: err %v, %d requests", err, doer.callCount())
	}
}

func TestOptionalRequestFailureIsLeftOut(t *testing.T) {
	doer := &fakeDoer{errs: map[string]error{"profile": errors.New("connection reset")}}
	p, _ := newTestProber(doer, claudeAuth("a1"))
	drain(p)
	entry := p.Entries()[0]
	if entry.Error != nil {
		t.Fatalf("Error = %+v, want none", entry.Error)
	}
	if _, ok := entry.Responses["profile"]; ok || len(entry.Responses) != 2 {
		t.Fatalf("responses = %v, want usage and reset_grants", entry.Responses)
	}
}

func TestRefreshRejectsUnknownAndUnsupportedCredentials(t *testing.T) {
	p, _ := newTestProber(&fakeDoer{}, &coreauth.Auth{Index: "g1", Provider: "gemini"})
	if _, err := p.Refresh(context.Background(), "nope"); !errors.Is(err, ErrUnknownAuth) {
		t.Fatalf("unknown auth: %v", err)
	}
	if _, err := p.Refresh(context.Background(), "g1"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("gemini auth: %v", err)
	}
}

func TestRequestedProbeGoesAheadOfScheduledOnes(t *testing.T) {
	p, _ := newTestProber(&fakeDoer{}, claudeAuth("a1"), claudeAuth("a2"), claudeAuth("a3"))
	p.enqueueDue()
	go func() { _, _ = p.Refresh(context.Background(), "a3") }()
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.queue) > 0 && p.queue[0] == "a3"
	})
	if first, _ := p.next(); first != "a3" || len(p.queue) != 2 {
		t.Fatalf("next = %q, want the requested a3", first)
	}
	p.probe(context.Background(), "a3")
}

func TestRunProbesOneAccountAtATime(t *testing.T) {
	auths := []*coreauth.Auth{claudeAuth("a1"), claudeAuth("a2"), {Index: "c1", Provider: "codex"}}
	doer := &fakeDoer{}
	p, _ := newTestProber(doer, auths...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	var wg sync.WaitGroup
	for _, auth := range auths {
		wg.Add(1)
		go func(authIndex string) {
			defer wg.Done()
			if _, err := p.Refresh(context.Background(), authIndex); err != nil {
				t.Errorf("Refresh(%s): %v", authIndex, err)
			}
		}(auth.Index)
	}
	wg.Wait()
	if got := doer.maxFlight.Load(); got != 1 {
		t.Fatalf("up to %d requests ran at once, want 1", got)
	}
	if len(p.Entries()) != 3 {
		t.Fatalf("Entries = %d, want 3", len(p.Entries()))
	}
}

func TestCodexPlanUsesTheAccountID(t *testing.T) {
	withID, _ := planFor(&coreauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acc-1"}})
	if len(withID) != 3 || withID[0].Header["Chatgpt-Account-Id"] != "acc-1" || withID[1].URL != codexSubscriptionURL+"?account_id=acc-1" ||
		withID[2].Header["Originator"] != "Codex Desktop" || withID[0].Header["Originator"] != "" {
		t.Fatalf("plan with account id = %+v", withID)
	}
	withoutID, _ := planFor(&coreauth.Auth{Provider: "codex"})
	if len(withoutID) != 2 || withoutID[1].Name != "reset_credits" {
		t.Fatalf("plan without account id = %+v", withoutID)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
