package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestGetRoutingRankingReportsPaceOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := quota.NewCache()
	manager := coreauth.NewManager(nil, coreauth.NewPaceSelector(coreauth.PaceConfig{BurstReserve: 0.2, PaceTolerance: 0.1, StaleAfter: time.Hour}, cache), nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "ahead", Provider: "claude", Status: coreauth.StatusActive, Index: "idx-ahead"},
		{ID: "behind", Provider: "claude", Status: coreauth.StatusActive, Index: "idx-behind"},
		{ID: "off", Provider: "claude", Status: coreauth.StatusDisabled, Disabled: true, Index: "idx-off"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	now := time.Now()
	window := func(remaining float64) []quota.Window {
		return []quota.Window{{ID: quota.SevenDay, Remaining: remaining, ResetAt: now.Add(84 * time.Hour), Length: 168 * time.Hour, ObservedAt: now, Source: quota.SourceProbe}}
	}
	cache.PutProbe("ahead", window(0.2))
	cache.PutProbe("behind", window(0.9))

	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "pace"}}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/ranking?provider=claude", nil)
	h.GetRoutingRanking(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}

	var body struct {
		Strategy string `json:"strategy"`
		Pools    []struct {
			Provider    string   `json:"provider"`
			Model       *string  `json:"model"`
			Ranked      bool     `json:"ranked"`
			LongWindows []string `json:"long_windows"`
			Entries     []struct {
				AuthIndex string   `json:"auth_index"`
				Rank      int      `json:"rank"`
				Class     string   `json:"class"`
				TopBand   bool     `json:"top_band"`
				Pace      *float64 `json:"pace"`
				ShortLeft *float64 `json:"short_remaining"`
			} `json:"entries"`
			NotCandidates []struct {
				AuthIndex string `json:"auth_index"`
				Reason    string `json:"reason"`
			} `json:"not_candidates"`
		} `json:"pools"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode: %v; body %s", errDecode, rec.Body)
	}
	if body.Strategy != "pace" || len(body.Pools) != 1 {
		t.Fatalf("body = %s", rec.Body)
	}
	pool := body.Pools[0]
	if pool.Provider != "claude" || pool.Model != nil || !pool.Ranked || len(pool.LongWindows) != 1 || pool.LongWindows[0] != "seven-day" {
		t.Fatalf("pool = %s", rec.Body)
	}
	if len(pool.Entries) != 2 || pool.Entries[0].AuthIndex != "idx-behind" || pool.Entries[0].Rank != 1 || !pool.Entries[0].TopBand || pool.Entries[0].Class != "ready" {
		t.Fatalf("entries = %s", rec.Body)
	}
	if pool.Entries[0].Pace == nil || *pool.Entries[0].Pace < 1.79 || *pool.Entries[0].Pace > 1.81 || pool.Entries[0].ShortLeft != nil {
		t.Fatalf("rank 1 pace/short = %s", rec.Body)
	}
	if len(pool.NotCandidates) != 1 || pool.NotCandidates[0].AuthIndex != "idx-off" || pool.NotCandidates[0].Reason != "disabled" {
		t.Fatalf("not candidates = %s", rec.Body)
	}
}

func TestGetRoutingRankingReportsTheRunway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	type poolJSON struct {
		Runway *struct {
			Reason        string     `json:"reason"`
			Speed         *float64   `json:"speed"`
			DelayMS       int64      `json:"delay_ms"`
			DryAt         *time.Time `json:"dry_at"`
			Window        *string    `json:"window"`
			Accounts      int        `json:"accounts"`
			DemandPerHour float64    `json:"demand_per_hour"`
			MaxDelayMS    int64      `json:"max_delay_ms"`
			Unsized       []string   `json:"unsized"`
		} `json:"runway"`
	}
	// rank serves one Claude account with half of each window left. With history, it spent
	// 300 units two days ago and 100 in the last hour, one request a minute: windows of 200
	// (5-hour) and 800 (7-day) units, and the next hour's 115 overrun the 5-hour window.
	rank := func(t *testing.T, throttle coreauth.ThrottleConfig, history bool) poolJSON {
		t.Helper()
		cache := quota.NewCache()
		manager := coreauth.NewManager(nil, coreauth.NewPaceSelector(coreauth.PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), nil)
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "a", Provider: "claude", Status: coreauth.StatusActive, Index: "idx-a"}); errRegister != nil {
			t.Fatal(errRegister)
		}
		cache.PutProbe("a", []quota.Window{
			{ID: quota.FiveHour, Remaining: 0.5, ResetAt: now.Add(150 * time.Minute), Length: 5 * time.Hour, ObservedAt: now, Source: quota.SourceProbe},
			{ID: quota.SevenDay, Remaining: 0.5, ResetAt: now.Add(84 * time.Hour), Length: 168 * time.Hour, ObservedAt: now, Source: quota.SourceProbe},
		})
		if history {
			entries := []coreauth.LedgerEntry{{At: now.Add(-48 * time.Hour), Provider: "claude", AuthIndex: "idx-a", Model: "claude-opus-5-5", Units: 300}}
			for i := range 60 {
				entries = append(entries, coreauth.LedgerEntry{At: now.Add(-time.Hour + time.Duration(i)*time.Minute), Provider: "claude", AuthIndex: "idx-a", Model: "claude-opus-5-5", SessionID: "s1", Units: 100.0 / 60})
			}
			manager.Ledger().Seed(now.Add(-8*24*time.Hour), entries)
		}
		h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "pace"}}, authManager: manager}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/ranking?provider=claude", nil)
		h.GetRoutingRanking(c)
		var body struct {
			Pools []poolJSON `json:"pools"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil || len(body.Pools) != 1 {
			t.Fatalf("decode: %v; body %s", errDecode, rec.Body)
		}
		return body.Pools[0]
	}

	on := coreauth.ThrottleConfig{MaxDelay: 120 * time.Second, Margin: 0.15}
	short := rank(t, on, true)
	r := short.Runway
	if r == nil || r.Reason != "short" || r.Speed == nil || *r.Speed <= 0 || *r.Speed >= 1 || r.DryAt == nil ||
		r.Window == nil || *r.Window != "seven-day" || r.Accounts != 1 || r.DemandPerHour < 100 || r.MaxDelayMS != 120_000 ||
		r.Unsized == nil || len(r.Unsized) != 0 {
		t.Fatalf("runway = %+v", r)
	}
	// One-minute turns at speed s wait 60 s × (1/s − 1), capped at max-delay.
	want := min(int64(60_000*(1 / *r.Speed - 1)), 120_000)
	if r.DelayMS < want-1_000 || r.DelayMS > want+1_000 {
		t.Fatalf("delay = %d ms, want about %d", r.DelayMS, want)
	}

	if off := rank(t, coreauth.ThrottleConfig{Margin: 0.15}, true).Runway; off == nil || off.Reason != "short" || off.DelayMS != 0 || off.MaxDelayMS != 0 {
		t.Fatalf("throttle off: runway = %+v", off)
	}
	if learning := rank(t, on, false).Runway; learning == nil || learning.Reason != "learning" || learning.Speed != nil || learning.DelayMS != 0 || learning.Window != nil {
		t.Fatalf("no history: runway = %+v", learning)
	}
}

func TestGetRoutingRankingOmitsTheRunwayForOtherStrategies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/routing/ranking?provider=claude", nil)
	h.GetRoutingRanking(c)
	var body struct {
		Pools []map[string]json.RawMessage `json:"pools"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil || len(body.Pools) != 1 {
		t.Fatalf("decode: %v; body %s", errDecode, rec.Body)
	}
	if string(body.Pools[0]["runway"]) != "null" {
		t.Fatalf("pool = %s", rec.Body)
	}
}
