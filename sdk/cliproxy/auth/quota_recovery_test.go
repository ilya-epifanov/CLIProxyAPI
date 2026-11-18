package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

const (
	recoveryOpus  = "claude-opus-5-5"
	recoveryFable = "claude-fable-5-1"
)

// newRecoveryManager registers one Claude account and applies results to it.
func newRecoveryManager(t *testing.T, results ...Result) (*Manager, string) {
	t.Helper()
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "claude-recovery.json", Provider: "claude"}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	for _, result := range results {
		result.AuthID, result.Provider = auth.ID, auth.Provider
		manager.MarkResult(context.Background(), result)
	}
	return manager, auth.ID
}

// claudeQuota429 is a 429 whose headers put the account in cooldown for five days.
func claudeQuota429(model string, credentialScope bool) Result {
	retryAfter := 5 * 24 * time.Hour
	return Result{
		Model:           model,
		CredentialScope: credentialScope,
		RetryAfter:      &retryAfter,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
	}
}

// readAfter is a send time after every cooldown the account has.
func readAfter(t *testing.T, manager *Manager, authID string) time.Time {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("account missing")
	}
	latest := auth.UpdatedAt
	for _, state := range auth.ModelStates {
		if state != nil && state.UpdatedAt.After(latest) {
			latest = state.UpdatedAt
		}
	}
	return latest.Add(time.Second)
}

func window(id quota.WindowID, remaining float64) quota.Window {
	return quota.Window{ID: id, Remaining: remaining, ResetAt: time.Now().Add(72 * time.Hour), Length: 7 * 24 * time.Hour}
}

func openClaudeWindows() []quota.Window {
	return []quota.Window{window(quota.FiveHour, 1), window(quota.SevenDay, 1), window(quota.SevenDayFable, 1)}
}

func cooling(t *testing.T, manager *Manager, authID string) bool {
	t.Helper()
	auth, _ := manager.GetByID(authID)
	return auth.Unavailable || auth.Quota.Exceeded || isModelStateActiveCooldown(auth.ModelStates[recoveryOpus], time.Now())
}

func TestReleaseRecoveredQuotaCooldownClearsWhenTheReadShowsOpenWindows(t *testing.T) {
	manager, authID := newRecoveryManager(t, claudeQuota429(recoveryOpus, true))
	if !cooling(t, manager, authID) {
		t.Fatal("the 429 did not start a cooldown")
	}

	released, err := manager.ReleaseRecoveredQuotaCooldown(context.Background(), authID, openClaudeWindows(), readAfter(t, manager, authID))
	if err != nil || !released {
		t.Fatalf("released = %v, err = %v; want true, nil", released, err)
	}
	if cooling(t, manager, authID) {
		t.Fatal("the account is still cooling down")
	}
}

func TestReleaseRecoveredQuotaCooldownKeepsTheCooldown(t *testing.T) {
	exhausted := func(id quota.WindowID) []quota.Window {
		out := openClaudeWindows()
		for i := range out {
			if out[i].ID == id {
				out[i].Remaining = 0
			}
		}
		return out
	}
	without := func(id quota.WindowID) []quota.Window {
		var out []quota.Window
		for _, w := range openClaudeWindows() {
			if w.ID != id {
				out = append(out, w)
			}
		}
		return out
	}
	serverError := Result{Model: "claude-sonnet-5-5", Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"}}

	tests := []struct {
		name    string
		results []Result
		windows []quota.Window
		// sentBefore sends the read before the latest result.
		sentBefore bool
	}{
		{name: "no cooldown", windows: openClaudeWindows()},
		{name: "seven-day still used up", results: []Result{claudeQuota429(recoveryOpus, true)}, windows: exhausted(quota.SevenDay)},
		{name: "five-hour still used up", results: []Result{claudeQuota429(recoveryOpus, true)}, windows: exhausted(quota.FiveHour)},
		{name: "seven-day not reported", results: []Result{claudeQuota429(recoveryOpus, true)}, windows: without(quota.SevenDay)},
		{name: "read sent before the 429", results: []Result{claudeQuota429(recoveryOpus, true)}, windows: openClaudeWindows(), sentBefore: true},
		{name: "Fable window still used up", results: []Result{claudeQuota429(recoveryFable, false)}, windows: exhausted(quota.SevenDayFable)},
		{name: "another model has a non-quota cooldown", results: []Result{claudeQuota429(recoveryOpus, false), serverError}, windows: openClaudeWindows()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, authID := newRecoveryManager(t, tt.results...)
			before, _ := manager.GetByID(authID)
			sentAt := readAfter(t, manager, authID)
			if tt.sentBefore {
				sentAt = sentAt.Add(-time.Hour)
			}

			released, err := manager.ReleaseRecoveredQuotaCooldown(context.Background(), authID, tt.windows, sentAt)
			if err != nil || released {
				t.Fatalf("released = %v, err = %v; want false, nil", released, err)
			}
			after, _ := manager.GetByID(authID)
			if after.Generation != before.Generation {
				t.Fatal("a refused release changed the account")
			}
		})
	}
}

func TestReleaseRecoveredQuotaCooldownClearsAFableCooldownOnceItsWindowOpens(t *testing.T) {
	manager, authID := newRecoveryManager(t, claudeQuota429(recoveryFable, false))

	released, err := manager.ReleaseRecoveredQuotaCooldown(context.Background(), authID, openClaudeWindows(), readAfter(t, manager, authID))
	if err != nil || !released {
		t.Fatalf("released = %v, err = %v; want true, nil", released, err)
	}
	auth, _ := manager.GetByID(authID)
	if isModelStateActiveCooldown(auth.ModelStates[recoveryFable], time.Now()) {
		t.Fatal("the Fable model is still cooling down")
	}
}
