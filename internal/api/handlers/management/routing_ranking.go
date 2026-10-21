package management

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

// rankedProviders are the providers ranked when the request names none.
var rankedProviders = []string{"claude", "codex"}

type rankingEntryJSON struct {
	AuthIndex       string           `json:"auth_index"`
	Rank            int              `json:"rank"`
	Priority        int              `json:"priority"`
	Class           string           `json:"class"`
	TopBand         bool             `json:"top_band"`
	LongWindows     []quota.WindowID `json:"long_windows"`
	Pace            *float64         `json:"pace"`
	PaceWindow      quota.WindowID   `json:"pace_window,omitempty"`
	ResetsAt        *time.Time       `json:"resets_at"`
	Estimated       bool             `json:"estimated"`
	ShortRemaining  *float64         `json:"short_remaining"`
	ExhaustedWindow quota.WindowID   `json:"exhausted_window,omitempty"`
	BackAt          *time.Time       `json:"back_at"`
	ObservedAt      *time.Time       `json:"observed_at"`
	Stale           bool             `json:"stale"`
}

type notCandidateJSON struct {
	AuthIndex string     `json:"auth_index"`
	Reason    string     `json:"reason"`
	Until     *time.Time `json:"until"`
}

type rankingPoolJSON struct {
	Provider string `json:"provider"`
	// Model is null for a credential-scope ranking.
	Model *string `json:"model"`
	// Ranked is false when the strategy does not rank; entries are then in ID order.
	Ranked bool `json:"ranked"`
	// LongWindows is the union of the entries' long windows, in routing table order.
	LongWindows   []quota.WindowID   `json:"long_windows"`
	Entries       []rankingEntryJSON `json:"entries"`
	NotCandidates []notCandidateJSON `json:"not_candidates"`
	// Runway is null unless the strategy is pace.
	Runway *runwayJSON `json:"runway"`
}

// runwayJSON is the pool's runway and the throttle it sets (coreauth.RunwayStatus).
type runwayJSON struct {
	// Reason is "clear", "short", "reserve" or "learning".
	Reason string `json:"reason"`
	// Speed is the share of the forecast the pool serves for a week; null while learning.
	Speed *float64 `json:"speed"`
	// DelayMS is the delay a request for this pool gets now; 0 when the throttle is off.
	DelayMS int64 `json:"delay_ms"`
	// DryAt is when the pool runs dry at full speed; null when it lasts a week.
	DryAt    *time.Time      `json:"dry_at"`
	Window   *quota.WindowID `json:"window"`
	Accounts int             `json:"accounts"`
	// DemandPerHour is the next hour's forecast in cost units, margin included.
	DemandPerHour float64 `json:"demand_per_hour"`
	// MaxDelayMS is 0 when the throttle is off.
	MaxDelayMS int64 `json:"max_delay_ms"`
	// Unsized lists the short windows the plan leaves out until an account sizes them.
	Unsized []quota.WindowID `json:"unsized"`
}

// GetRoutingRanking returns how the current selector would order each provider's
// accounts for a cold pick: GET /routing/ranking[?provider=][&model=].
func (h *Handler) GetRoutingRanking(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth_manager_unavailable"})
		return
	}
	providers := rankedProviders
	if provider := strings.ToLower(strings.TrimSpace(c.Query("provider"))); provider != "" {
		providers = []string{provider}
	}
	model := strings.TrimSpace(c.Query("model"))
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	if !ok {
		strategy = strings.TrimSpace(h.cfg.Routing.Strategy)
	}

	pools := make([]rankingPoolJSON, 0, len(providers))
	for _, provider := range providers {
		ranking := h.authManager.RankCandidates(c.Request.Context(), provider, model)
		pool := rankingPoolJSON{
			Provider:      provider,
			Ranked:        ranking.Ranked,
			LongWindows:   []quota.WindowID{},
			Entries:       make([]rankingEntryJSON, 0, len(ranking.Entries)),
			NotCandidates: make([]notCandidateJSON, 0, len(ranking.NotCandidates)),
		}
		if model != "" {
			pool.Model = &model
		}
		if r := ranking.Runway; r != nil {
			pool.Runway = &runwayJSON{
				Reason:        string(r.Reason),
				Speed:         floatPtr(r.Speed),
				DelayMS:       r.Delay.Milliseconds(),
				DryAt:         timePtr(r.DryAt),
				Accounts:      r.Accounts,
				DemandPerHour: r.Demand,
				MaxDelayMS:    r.Config.MaxDelay.Milliseconds(),
				Unsized:       append([]quota.WindowID{}, r.Unsized...),
			}
			if r.Window != "" {
				window := r.Window
				pool.Runway.Window = &window
			}
		}
		seen := map[quota.WindowID]bool{}
		for i, entry := range ranking.Entries {
			pool.Entries = append(pool.Entries, rankingEntryFor(i+1, entry))
			for _, id := range entry.LongWindows {
				if !seen[id] {
					seen[id] = true
					pool.LongWindows = append(pool.LongWindows, id)
				}
			}
		}
		for _, nc := range ranking.NotCandidates {
			pool.NotCandidates = append(pool.NotCandidates, notCandidateJSON{
				AuthIndex: nc.Auth.EnsureIndex(),
				Reason:    nc.Reason,
				Until:     timePtr(nc.Until),
			})
		}
		pools = append(pools, pool)
	}
	c.JSON(http.StatusOK, gin.H{
		"strategy":     strategy,
		"generated_at": time.Now().UTC(),
		"pools":        pools,
	})
}

func rankingEntryFor(rank int, entry coreauth.Ranked) rankingEntryJSON {
	out := rankingEntryJSON{
		AuthIndex:       entry.Auth.EnsureIndex(),
		Rank:            rank,
		Priority:        entry.Priority,
		Class:           entry.Class.String(),
		TopBand:         entry.TopBand,
		LongWindows:     entry.LongWindows,
		Pace:            floatPtr(entry.Pace),
		PaceWindow:      entry.PaceWindow,
		ResetsAt:        timePtr(entry.ResetAt),
		Estimated:       entry.Estimated,
		ShortRemaining:  floatPtr(entry.ShortRemaining),
		ExhaustedWindow: entry.ExhaustedWindow,
		BackAt:          timePtr(entry.BackAt),
		ObservedAt:      timePtr(entry.ObservedAt),
		Stale:           entry.Stale,
	}
	if out.LongWindows == nil {
		out.LongWindows = []quota.WindowID{}
	}
	return out
}

func floatPtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
