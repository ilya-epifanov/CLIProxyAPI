package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestThrottleOverrideRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, coreauth.NewPaceSelector(coreauth.PaceConfig{PaceTolerance: 0.1, StaleAfter: time.Hour, Throttle: coreauth.ThrottleConfig{MaxDelay: time.Minute}}, quota.NewCache()), nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "a", Provider: "claude", Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "pace"}}, authManager: manager}
	engine := gin.New()
	engine.PUT("/routing/throttle/overrides/:provider", h.PutThrottleOverride)
	engine.DELETE("/routing/throttle/overrides/:provider", h.DeleteThrottleOverride)
	engine.GET("/routing/ranking", h.GetRoutingRanking)
	serve := func(method, url, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
		return rec
	}
	type runway struct {
		Override *struct {
			Speed float64 `json:"speed"`
		} `json:"override"`
	}
	overrideOf := func() *float64 {
		var body struct {
			Pools []struct {
				Runway *runway `json:"runway"`
			} `json:"pools"`
		}
		rec := serve(http.MethodGet, "/routing/ranking?provider=claude", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Pools) != 1 || body.Pools[0].Runway == nil {
			t.Fatalf("ranking: %v, %s", err, rec.Body)
		}
		if o := body.Pools[0].Runway.Override; o != nil {
			return &o.Speed
		}
		return nil
	}

	for _, tc := range []struct{ url, body string }{
		{"/routing/throttle/overrides/claude", `{}`},
		{"/routing/throttle/overrides/claude", `{"speed":1.5}`},
		{"/routing/throttle/overrides/claude", `{"speed":-0.1}`},
	} {
		if rec := serve(http.MethodPut, tc.url, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s %s: status %d, want 400", tc.url, tc.body, rec.Code)
		}
	}
	if rec := serve(http.MethodPut, "/routing/throttle/overrides/xai", `{"speed":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown pool: status %d, want 404", rec.Code)
	}

	if rec := serve(http.MethodPut, "/routing/throttle/overrides/claude", `{"speed":0.25}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status %d, body %s", rec.Code, rec.Body)
	}
	if got := overrideOf(); got == nil || *got != 0.25 {
		t.Fatalf("ranking override = %v; want 0.25", got)
	}
	if rec := serve(http.MethodDelete, "/routing/throttle/overrides/claude", ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: status %d, body %s", rec.Code, rec.Body)
	}
	if got := overrideOf(); got != nil {
		t.Fatalf("ranking override = %v after DELETE; want none", *got)
	}
}
