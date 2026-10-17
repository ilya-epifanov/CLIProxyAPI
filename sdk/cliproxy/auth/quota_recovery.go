package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

// ReleaseRecoveredQuotaCooldown clears the cooldown of authID when a quota read
// shows the account is usable again before the cooldown deadline, for example
// after a reset that did not go through this proxy. windows are the read's
// windows and sentAt is when the read was sent.
//
// It clears only when every active cooldown of the account is a quota cooldown
// that started before sentAt, and the read shows every window that cooldown
// covers as open (see quotaCooldownsRecovered). Otherwise nothing changes. It
// reports whether it cleared.
func (m *Manager) ReleaseRecoveredQuotaCooldown(ctx context.Context, authID string, windows []quota.Window, sentAt time.Time) (bool, error) {
	if m == nil || len(windows) == 0 {
		return false, nil
	}
	updated, _, err := m.resetQuotaIf(ctx, authID, func(auth *Auth, now time.Time) bool {
		return quotaCooldownsRecovered(auth, windows, sentAt, now)
	})
	if updated == nil || err != nil {
		return false, err
	}
	log.WithFields(log.Fields{"auth_id": authID, "provider": updated.Provider}).
		Info("quota read shows the account's windows open again; cleared its quota cooldown")
	return true, nil
}

// quotaCooldownsRecovered reports whether auth has at least one active cooldown,
// and every active cooldown
//   - is a quota cooldown (a 429), not an auth, model-support or server error;
//   - started before sentAt, so the read cannot predate the 429 that set it;
//   - covers only windows the read shows open: the routing windows of the
//     cooled-down model (quota.RoutingWindowsFor), or of no model for the
//     credential-wide cooldown. A missing short window counts as open, a missing
//     long window as unknown, as in the pace selector.
func quotaCooldownsRecovered(auth *Auth, windows []quota.Window, sentAt, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	read := make(map[quota.WindowID]quota.Window, len(windows))
	for _, w := range windows {
		read[w.ID] = w
	}
	open := func(model string) bool {
		routing, tracked := quota.RoutingWindowsFor(auth.Provider, model)
		if !tracked {
			return false
		}
		if w, ok := read[routing.Short]; ok && exhausted(w, now) {
			return false
		}
		for _, slot := range routing.Long {
			w, ok := firstWindow(read, slot)
			if !ok || exhausted(w, now) {
				return false
			}
		}
		return true
	}

	cooling := false
	var startedAt time.Time
	for model, state := range auth.ModelStates {
		if state == nil || !isModelStateActiveCooldown(state, now) {
			continue
		}
		if !isQuotaCooldown(state.Quota) || !open(canonicalModelKey(model)) {
			return false
		}
		cooling = true
		if state.UpdatedAt.After(startedAt) {
			startedAt = state.UpdatedAt
		}
	}
	if authCooling := auth.NextRetryAfter.After(now) || auth.Quota.NextRecoverAt.After(now); authCooling {
		if !isQuotaCooldown(auth.Quota) || !open("") {
			return false
		}
		// A model state records when its 429 arrived. A credential-wide cooldown
		// always comes with one, so auth.UpdatedAt is only the fallback.
		if !cooling {
			startedAt = auth.UpdatedAt
		}
		cooling = true
	}
	return cooling && startedAt.Before(sentAt)
}

// isQuotaCooldown reports a cooldown set by a 429 (MarkResult's 429 branch).
func isQuotaCooldown(q QuotaState) bool {
	reason := strings.TrimSpace(q.Reason)
	return q.Exceeded && (reason == "quota" || reason == "credential_quota")
}

func firstWindow(read map[quota.WindowID]quota.Window, slot quota.Slot) (quota.Window, bool) {
	for _, id := range slot {
		if w, ok := read[id]; ok {
			return w, true
		}
	}
	return quota.Window{}, false
}
