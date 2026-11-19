// Package quotaprobe reads Claude and Codex account quota from the providers'
// private usage endpoints on the server, on one schedule and behind one rate
// limiter, and keeps the latest responses for the management panel.
//
// The panel parses the responses with the same code it uses for direct reads,
// so this package only decides which requests to send and when.
package quotaprobe

import (
	"maps"
	"net/url"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Request is one upstream GET of a probe. The Doer adds the account's
// Authorization header.
type Request struct {
	// Name identifies the response for the panel, e.g. "usage".
	Name   string
	URL    string
	Header map[string]string
}

// Requests and headers mirror the panel's direct reads
// (panel src/utils/quota/constants.ts, src/services/api/claudeResetGrants.ts).
const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL     = "https://api.anthropic.com/api/oauth/profile"
	claudeResetGrantsURL = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"

	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexSubscriptionURL = "https://chatgpt.com/backend-api/subscriptions"
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

// planFor returns the requests that read auth's quota, primary request first,
// or false for a provider this package does not probe. A probe succeeds when
// the primary request returns 2xx.
func planFor(auth *coreauth.Auth) ([]Request, bool) {
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		header := map[string]string{
			"User-Agent":     "claude-cli/2.1.280 (external, cli)",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		}
		return []Request{
			{Name: "usage", URL: claudeUsageURL, Header: header},
			{Name: "profile", URL: claudeProfileURL, Header: header},
			{Name: "reset_grants", URL: claudeResetGrantsURL, Header: header},
		}, true
	case "codex":
		accountID := codexAccountID(auth)
		header := map[string]string{
			"Content-Type": "application/json",
			"User-Agent":   "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
		}
		if accountID != "" {
			header["Chatgpt-Account-Id"] = accountID
		}
		plan := []Request{{Name: "usage", URL: codexUsageURL, Header: header}}
		if accountID != "" {
			plan = append(plan, Request{
				Name:   "subscription",
				URL:    codexSubscriptionURL + "?account_id=" + url.QueryEscape(accountID),
				Header: header,
			})
		}
		resetHeader := maps.Clone(header)
		resetHeader["Accept"] = "application/json"
		resetHeader["OpenAI-Beta"] = "codex-1"
		resetHeader["Originator"] = "Codex Desktop"
		return append(plan, Request{Name: "reset_credits", URL: codexResetCreditsURL, Header: resetHeader}), true
	default:
		return nil, false
	}
}

func codexAccountID(auth *coreauth.Auth) string {
	if id, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return strings.TrimSpace(auth.Attributes["account_id"])
}
