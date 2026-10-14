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
	targetKey := canonicalSchedulingProvider(provider)
	model = strings.TrimSpace(model)
	registryRef := registry.GetGlobalRegistry()

	m.mu.RLock()
	selector := m.selector
	var candidates []*Auth
	var out Ranking
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
			out.NotCandidates = append(out.NotCandidates, NotCandidate{
				Auth:   auth.Clone(),
				Reason: notCandidateReason(auth, reason),
				Until:  until,
			})
			continue
		}
		candidates = append(candidates, auth.Clone())
	}
	selectionModels := m.selectionModelsLocked(selector, candidates, model)
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
