package quotaprobe

import (
	"net/http"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

const claudeUsageBody = `{"five_hour":{"utilization":20,"resets_at":"2026-10-11T12:00:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-10-15T10:00:00Z"}}`

func TestProbeStoresParsedWindowsByAuthID(t *testing.T) {
	doer := &fakeDoer{responses: map[string]Response{"usage": {StatusCode: http.StatusOK, Body: claudeUsageBody}}}
	auth := claudeAuth("a1")
	auth.ID = "claude-a1.json"
	cache := quota.NewCache()
	p, _ := newTestProber(doer, auth)
	p.cache = cache

	drain(p)
	snapshot, ok := cache.Lookup("claude-a1.json", "claude", nil, time.Time{})
	if !ok {
		t.Fatal("probe did not reach the cache")
	}
	if w, found := snapshot.Window(quota.SevenDay); !found || w.Remaining != 0.6 || w.Source != quota.SourceProbe {
		t.Fatalf("seven-day = %+v, %v", w, found)
	}

	// A parse failure keeps the previous windows.
	doer.responses["usage"] = Response{StatusCode: http.StatusOK, Body: "not json"}
	if _, errRefresh := p.Refresh(t.Context(), "a1"); errRefresh != nil {
		t.Fatalf("Refresh: %v", errRefresh)
	}
	if _, ok := cache.Lookup("claude-a1.json", "claude", nil, time.Time{}); !ok {
		t.Fatal("unparsable response dropped the cached windows")
	}
}

func TestProbeForgetsRemovedAccountsInCache(t *testing.T) {
	doer := &fakeDoer{responses: map[string]Response{"usage": {StatusCode: http.StatusOK, Body: claudeUsageBody}}}
	auths := []*coreauth.Auth{claudeAuth("a1")}
	auths[0].ID = "claude-a1.json"
	cache := quota.NewCache()
	p, _ := newTestProber(doer)
	p.auths = func() []*coreauth.Auth {
		out := make([]*coreauth.Auth, len(auths))
		for i, auth := range auths {
			out[i] = auth.Clone()
		}
		return out
	}
	p.cache = cache

	drain(p)
	auths = nil
	p.enqueueDue()
	if _, ok := cache.Lookup("claude-a1.json", "claude", nil, time.Time{}); ok {
		t.Fatal("removed account kept its cached windows")
	}
}

func TestRequestRefreshQueuesDueAccountsOnly(t *testing.T) {
	doer := &fakeDoer{}
	auth := claudeAuth("a1")
	auth.ID = "claude-a1.json"
	p, c := newTestProber(doer, auth)

	drain(p)
	calls := doer.callCount()
	p.RequestRefresh("claude-a1.json")
	p.enqueueRequested()
	if _, queued := p.next(); queued {
		t.Fatal("a request within MinInterval must not queue a probe")
	}

	c.Advance(6 * time.Minute)
	p.RequestRefresh("claude-a1.json")
	p.RequestRefresh("unknown.json")
	p.enqueueRequested()
	authIndex, queued := p.next()
	if !queued || authIndex != "a1" {
		t.Fatalf("next = %q, %v; want a1", authIndex, queued)
	}
	if _, more := p.next(); more {
		t.Fatal("unknown auth IDs must not queue anything")
	}
	if doer.callCount() != calls {
		t.Fatal("RequestRefresh must not probe on the caller's goroutine")
	}
}

func TestRequestRefreshNeverBlocks(t *testing.T) {
	p, _ := newTestProber(&fakeDoer{})
	for i := 0; i < refreshRequests*3; i++ {
		p.RequestRefresh("claude-a1.json")
	}
}
