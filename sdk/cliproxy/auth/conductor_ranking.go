package auth

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// NotCandidate is an account the selector cannot pick right now.
type NotCandidate struct {
	Auth *Auth
	// Reason is "disabled", "auth_error", "cooldown" or "unavailable".
	Reason string
	// Until is when a cooldown ends; zero when unknown or not applicable.
	Until time.Time
}

// Ranking explains how the current selector orders one provider's accounts.
type Ranking struct {
	// Entries are the candidates: tiers in descending priority, the selector's order
	// inside each tier. TopBand is set only in the highest tier, the only one a cold
	// pick reaches.
	Entries       []Ranked
	NotCandidates []NotCandidate
	// Ranked is false when the current selector is not a Ranker; Entries are then in ID
	// order with ClassUnknown.
	Ranked bool
	// Pool is the pace of all candidates together; nil unless the strategy is pace.
	Pool *PoolPace
	// Throttle is set when the pace strategy throttles requests.
	Throttle *ThrottleStatus
}

// ThrottleStatus is the throttle's settings and the delay a request gets now.
type ThrottleStatus struct {
	Config ThrottleConfig
	Delay  time.Duration
}

// rankerOf returns the Ranker behind selector, looking through session affinity.
func rankerOf(selector Selector) (Ranker, bool) {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	ranker, ok := selector.(Ranker)
	return ranker, ok
}

// RankCandidates ranks the accounts of provider for model the way a cold pick would,
// across all priority tiers. With model empty, only credential-wide blocks exclude an
// account and long windows are those of a model without a scoped window.
func (m *Manager) RankCandidates(ctx context.Context, provider, model string) Ranking {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	model = strings.TrimSpace(model)

	m.mu.RLock()
	selector := m.selector
	var out Ranking
	candidates, notCandidates := m.rankingCandidatesLocked(provider, model, now)
	out.NotCandidates = notCandidates
	selectionModels := m.selectionModelsLocked(selector, candidates, model)
	if pool, pace, ok := m.poolPaceLocked(ctx, []string{provider}, model, now); ok {
		out.Pool = &pool
		if throttle := pace.Throttle(); throttle.Enabled() {
			out.Throttle = &ThrottleStatus{Config: throttle, Delay: throttle.Delay(pool.Pace)}
		}
	}
	m.mu.RUnlock()

	sort.Slice(out.NotCandidates, func(i, j int) bool { return out.NotCandidates[i].Auth.ID < out.NotCandidates[j].Auth.ID })
	ctx = withSelectionModels(ctx, selectionModels)
	ranker, ranked := rankerOf(selector)
	out.Ranked = ranked

	byTier := map[int][]*Auth{}
	for _, auth := range candidates {
		byTier[authPriority(auth)] = append(byTier[authPriority(auth)], auth)
	}
	tiers := make([]int, 0, len(byTier))
	for priority := range byTier {
		tiers = append(tiers, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(tiers)))
	for i, priority := range tiers {
		tier := byTier[priority]
		var entries []Ranked
		if ranked {
			entries = ranker.Rank(ctx, tier, model, now)
		} else {
			entries = unrankedEntries(tier)
		}
		if i > 0 {
			for j := range entries {
				entries[j].TopBand = false
			}
		}
		out.Entries = append(out.Entries, entries...)
	}
	return out
}

// rankingCandidatesLocked splits provider's accounts that serve model into candidates
// and accounts blocked right now, both as clones. With model empty, only credential-wide
// blocks count. The caller holds m.mu.
func (m *Manager) rankingCandidatesLocked(provider, model string, now time.Time) ([]*Auth, []NotCandidate) {
	targetKey := canonicalSchedulingProvider(provider)
	model = strings.TrimSpace(model)
	registryRef := registry.GetGlobalRegistry()
	var candidates []*Auth
	var notCandidates []NotCandidate
	for _, auth := range m.auths {
		if auth == nil || canonicalSchedulingProvider(executorKeyFromAuth(auth)) != targetKey {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		checkModel := ""
		if model != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		if blocked, reason, until := isAuthBlockedForModel(auth, checkModel, now); blocked {
			notCandidates = append(notCandidates, NotCandidate{
				Auth:   auth.Clone(),
				Reason: notCandidateReason(auth, reason),
				Until:  until,
			})
			continue
		}
		candidates = append(candidates, auth.Clone())
	}
	return candidates, notCandidates
}

func notCandidateReason(auth *Auth, reason blockReason) string {
	switch {
	case reason == blockReasonDisabled:
		return "disabled"
	case reason == blockReasonCooldown:
		return "cooldown"
	case hasUnauthorizedAuthFailure(auth):
		return "auth_error"
	default:
		return "unavailable"
	}
}

func unrankedEntries(auths []*Auth) []Ranked {
	sorted := append([]*Auth(nil), auths...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]Ranked, 0, len(sorted))
	for _, auth := range sorted {
		out = append(out, Ranked{Auth: auth, Priority: authPriority(auth), Class: ClassUnknown, Pace: math.NaN(), ShortRemaining: math.NaN()})
	}
	return out
}
