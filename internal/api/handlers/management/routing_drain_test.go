package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestRoutingDrainRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, coreauth.NewPaceSelector(coreauth.PaceConfig{PaceTolerance: 0.1, StaleAfter: time.Hour}, quota.NewCache()), nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "a", Provider: "claude", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	index := auth.EnsureIndex()
	h := &Handler{cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "pace"}}, authManager: manager}
	engine := gin.New()
	engine.PUT("/routing/drain/:auth_index", h.PutRoutingDrain)
	engine.GET("/routing/ranking", h.GetRoutingRanking)
	serve := func(method, url, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(method, url, strings.NewReader(body)))
		return rec
	}
	rankedDrain := func() bool {
		var body struct {
			Pools []struct {
				Entries []struct {
					AuthIndex string `json:"auth_index"`
					Drain     *bool  `json:"drain"`
				} `json:"entries"`
			} `json:"pools"`
		}
		rec := serve(http.MethodGet, "/routing/ranking?provider=claude", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Pools) != 1 || len(body.Pools[0].Entries) != 1 {
			t.Fatalf("ranking: %v, %s", err, rec.Body)
		}
		entry := body.Pools[0].Entries[0]
		if entry.AuthIndex != index || entry.Drain == nil {
			t.Fatalf("entry = %+v; want %s with drain", entry, index)
		}
		return *entry.Drain
	}
	put := func(body string) *httptest.ResponseRecorder {
		return serve(http.MethodPut, "/routing/drain/"+index, body)
	}

	for _, body := range []string{``, `{}`, `{"drain":"yes"}`, `{"drain":null}`} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q: status %d, want 400", body, rec.Code)
		}
	}
	if rec := serve(http.MethodPut, "/routing/drain/missing", `{"drain":true}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"auth not found"`) {
		t.Errorf("unknown auth: status %d, body %s; want 404 auth not found", rec.Code, rec.Body)
	}
	if rec := put(`{"drain":false}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing an unflagged account: status %d, body %s", rec.Code, rec.Body)
	}
	if rankedDrain() {
		t.Fatal("ranking drain = true before the flag was set")
	}

	rec := put(`{"drain":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status %d, body %s", rec.Code, rec.Body)
	}
	var reply struct {
		AuthIndex string `json:"auth_index"`
		Drain     bool   `json:"drain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.AuthIndex != index || !reply.Drain {
		t.Fatalf("reply = %s; want auth_index %s and drain true", rec.Body, index)
	}
	if !manager.DrainFlags().Has("a") {
		t.Fatal("flag not stored by auth ID")
	}
	if !rankedDrain() {
		t.Fatal("ranking drain = false after the flag was set")
	}
	if rec := put(`{"drain":false}`); rec.Code != http.StatusOK || rankedDrain() {
		t.Fatalf("clear: status %d, body %s; want 200 and drain false", rec.Code, rec.Body)
	}
}
