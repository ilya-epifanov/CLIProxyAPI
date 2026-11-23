package auth

import (
	"context"
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
	// Runway is the pool's runway and the throttle it sets; nil unless the strategy is pace.
	Runway *RunwayStatus
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
	candidates, notCandidates, selectionModels := m.rankingCandidatesLocked(provider, model, now)
	out.NotCandidates = notCandidates
	pace, pools, paced := m.observeRunwayLocked([]string{provider}, model, now)
	drain := paced && m.drainServesLocked(pace, []string{provider}, model, now)
	m.mu.RUnlock()

	if paced {
		runway := m.planRunway(pace, []string{provider}, pools, drain, now)
		out.Runway = &runway
	}
	if _, ok := paceSelectorOf(selector); !ok {
		// Only the pace selector ranks by selection model.
		selectionModels = nil
	}

	sort.Slice(out.NotCandidates, func(i, j int) bool { return out.NotCandidates[i].Auth.ID < out.NotCandidates[j].Auth.ID })
	ctx = withSelectionModels(ctx, selectionModels)
	ctx = withDrainFlags(ctx, m.drains)
	ranker, ranked := behindAffinity(selector).(Ranker)
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
			entries = unrankedEntries(tier, m.drains)
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

// rankingAccount is one of a provider's accounts as m.auths holds it, not cloned, so it
// is read only while m.mu is held.
type rankingAccount struct {
	auth *Auth
	// model is the account's selection model; empty when the route model is.
	model string
	// blocked: the account cannot serve model right now, for reason until until.
	blocked bool
	reason  blockReason
	until   time.Time
}

// rankingPoolLocked lists provider's accounts that serve model, with their selection
// models and blocks. With model empty, only credential-wide blocks count. The caller
// holds m.mu.
func (m *Manager) rankingPoolLocked(provider, model string, now time.Time) []rankingAccount {
	targetKey := canonicalSchedulingProvider(provider)
	model = strings.TrimSpace(model)
	registryRef := registry.GetGlobalRegistry()
	var out []rankingAccount
	for _, auth := range m.auths {
		if auth == nil || canonicalSchedulingProvider(executorKeyFromAuth(auth)) != targetKey {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		account := rankingAccount{auth: auth}
		if model != "" {
			account.model = m.selectionModelForAuth(auth, model)
		}
		account.blocked, account.reason, account.until = isAuthBlockedForModel(auth, account.model, now)
		out = append(out, account)
	}
	return out
}

// rankingCandidatesLocked splits provider's accounts that serve model into candidates
// and accounts blocked right now, both as clones, and maps each account's ID to its
// selection model (nil with model empty). With model empty, only credential-wide blocks
// count. The caller holds m.mu.
func (m *Manager) rankingCandidatesLocked(provider, model string, now time.Time) ([]*Auth, []NotCandidate, map[string]string) {
	accounts := m.rankingPoolLocked(provider, model, now)
	var selectionModels map[string]string
	if strings.TrimSpace(model) != "" {
		selectionModels = make(map[string]string, len(accounts))
	}
	var candidates []*Auth
	var notCandidates []NotCandidate
	for _, account := range accounts {
		if selectionModels != nil {
			selectionModels[account.auth.ID] = account.model
		}
		if account.blocked {
			notCandidates = append(notCandidates, NotCandidate{
				Auth:   account.auth.Clone(),
				Reason: notCandidateReason(account.auth, account.reason),
				Until:  account.until,
			})
			continue
		}
		candidates = append(candidates, account.auth.Clone())
	}
	return candidates, notCandidates, selectionModels
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

func unrankedEntries(auths []*Auth, drains *DrainFlags) []Ranked {
	sorted := append([]*Auth(nil), auths...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]Ranked, 0, len(sorted))
	for _, auth := range sorted {
		entry := unknownRanked(auth)
		entry.Drain = drains.Has(auth.ID)
		out = append(out, entry)
	}
	return out
}
