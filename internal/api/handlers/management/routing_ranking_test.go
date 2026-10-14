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
