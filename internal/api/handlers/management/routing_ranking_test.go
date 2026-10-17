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

func TestGetRoutingRankingReportsPoolPaceAndThrottle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now()
	window := func(remaining float64) []quota.Window {
		return []quota.Window{{ID: quota.SevenDay, Remaining: remaining, ResetAt: now.Add(84 * time.Hour), Length: 168 * time.Hour, ObservedAt: now, Source: quota.SourceProbe}}
	}
	type poolJSON struct {
		PoolPace *struct {
			Pace     *float64 `json:"pace"`
			Window   *string  `json:"window"`
			Accounts int      `json:"accounts"`
		} `json:"pool_pace"`
		Throttle *struct {
			DelayMS    int64   `json:"delay_ms"`
			Threshold  float64 `json:"threshold"`
			Floor      float64 `json:"floor"`
			MaxDelayMS int64   `json:"max_delay_ms"`
		} `json:"throttle"`
	}
	rank := func(t *testing.T, throttle coreauth.ThrottleConfig, remaining ...float64) poolJSON {
		t.Helper()
		cache := quota.NewCache()
		manager := coreauth.NewManager(nil, coreauth.NewPaceSelector(coreauth.PaceConfig{PaceTolerance: 0.1, Throttle: throttle}, cache), nil)
		for i, r := range remaining {
			id := string(rune('a' + i))
			if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Status: coreauth.StatusActive, Index: "idx-" + id}); errRegister != nil {
				t.Fatal(errRegister)
			}
			cache.PutProbe(id, window(r))
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

	// 0.2 + 0.1 remaining over 0.5 + 0.5 of the week left: pool pace 0.3, which is 2/3 of
	// the way from the 0.8 threshold to the 0.05 floor, so 80 s of the 120 s maximum.
	throttled := rank(t, coreauth.ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: 120 * time.Second}, 0.2, 0.1)
	if throttled.PoolPace == nil || throttled.PoolPace.Pace == nil || *throttled.PoolPace.Pace < 0.299 || *throttled.PoolPace.Pace > 0.301 ||
		throttled.PoolPace.Window == nil || *throttled.PoolPace.Window != "seven-day" || throttled.PoolPace.Accounts != 2 {
		t.Fatalf("pool_pace = %+v", throttled.PoolPace)
	}
	if throttled.Throttle == nil || throttled.Throttle.DelayMS < 79_900 || throttled.Throttle.DelayMS > 80_100 ||
		throttled.Throttle.Threshold != 0.8 || throttled.Throttle.Floor != 0.05 || throttled.Throttle.MaxDelayMS != 120_000 {
		t.Fatalf("throttle = %+v", throttled.Throttle)
	}

	off := rank(t, coreauth.ThrottleConfig{Threshold: 0.8, Floor: 0.05}, 0.2)
	if off.PoolPace == nil || off.Throttle != nil {
		t.Fatalf("throttle off: pool_pace = %+v, throttle = %+v", off.PoolPace, off.Throttle)
	}
	empty := rank(t, coreauth.ThrottleConfig{Threshold: 0.8, Floor: 0.05, MaxDelay: time.Minute})
	if empty.PoolPace == nil || empty.PoolPace.Pace != nil || empty.PoolPace.Window != nil || empty.PoolPace.Accounts != 0 || empty.Throttle == nil || empty.Throttle.DelayMS != 0 {
		t.Fatalf("no data: pool_pace = %+v, throttle = %+v", empty.PoolPace, empty.Throttle)
	}
}

func TestGetRoutingRankingOmitsPoolPaceForOtherStrategies(t *testing.T) {
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
	if string(body.Pools[0]["pool_pace"]) != "null" || string(body.Pools[0]["throttle"]) != "null" {
		t.Fatalf("pool = %s", rec.Body)
	}
}
