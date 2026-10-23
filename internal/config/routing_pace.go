package config

import (
	"fmt"
	"time"
)

// Defaults of the "pace" routing strategy.
const (
	DefaultPaceBurstReserve = 0.2
	DefaultPaceTolerance    = 0.1
	DefaultPaceStaleAfter   = 15 * time.Minute
	DefaultThrottleMargin   = 0.15
	DefaultThrottleGuard    = 12 * time.Hour
	routingPaceConfigPrefix = "routing.pace."
	// throttleWarnDelay is where a throttle delay nears the 300 s that Claude Code,
	// Codex and pi wait for a response before they give up.
	throttleWarnDelay = 240 * time.Second
)

// PaceRoutingConfig tunes the "pace" strategy, which sends new sessions to the account
// furthest behind schedule on its long (weekly) quota windows. Unset fields use the
// defaults above.
type PaceRoutingConfig struct {
	// BurstReserve is the 5-hour remaining fraction below which an account ranks after
	// every account with more; 0..1 exclusive of 1. 0 turns the reserve off.
	BurstReserve *float64 `yaml:"burst-reserve,omitempty" json:"burst-reserve,omitempty"`
	// PaceTolerance is the width of the top band, as a fraction of the best pace: a cold
	// pick chooses at random among accounts within it. 0..1 exclusive of 1.
	PaceTolerance *float64 `yaml:"pace-tolerance,omitempty" json:"pace-tolerance,omitempty"`
	// StaleAfter marks quota observations older than this as stale in the ranking.
	StaleAfter time.Duration `yaml:"stale-after,omitempty" json:"stale-after,omitempty"`
	// Throttle slows requests down so the pool does not run dry.
	Throttle PaceThrottleConfig `yaml:"throttle,omitempty" json:"throttle,omitempty"`
}

// PaceThrottleConfig slows requests down only as much as keeps the pool from running
// dry over the next week at the forecast demand (docs/frontend-design.md §4.7). A
// MaxDelay of 0 turns it off.
type PaceThrottleConfig struct {
	// MaxDelay is the longest wait added to one request. 0 (default) turns throttling off.
	MaxDelay time.Duration `yaml:"max-delay,omitempty" json:"max-delay,omitempty"`
	// Margin inflates the forecast demand, for error in the forecast and the quota
	// sizes; [0, 1].
	Margin *float64 `yaml:"margin,omitempty" json:"margin,omitempty"`
	// GuardHorizon is how far ahead the guard looks: what the usable accounts hold now
	// must last, at the burn the quota reads show, until its window refills or for this
	// long. Unset is the default; 0s turns the guard off.
	GuardHorizon *time.Duration `yaml:"guard-horizon,omitempty" json:"guard-horizon,omitempty"`
}

// Validate rejects values outside their ranges.
func (p PaceRoutingConfig) Validate() error {
	if p.BurstReserve != nil && (*p.BurstReserve < 0 || *p.BurstReserve >= 1) {
		return fmt.Errorf("%sburst-reserve must be in [0, 1), got %v", routingPaceConfigPrefix, *p.BurstReserve)
	}
	if p.PaceTolerance != nil && (*p.PaceTolerance < 0 || *p.PaceTolerance >= 1) {
		return fmt.Errorf("%space-tolerance must be in [0, 1), got %v", routingPaceConfigPrefix, *p.PaceTolerance)
	}
	if p.StaleAfter < 0 {
		return fmt.Errorf("%sstale-after must not be negative, got %s", routingPaceConfigPrefix, p.StaleAfter)
	}
	return p.Throttle.validate()
}

func (t PaceThrottleConfig) validate() error {
	const prefix = routingPaceConfigPrefix + "throttle."
	if margin := t.MarginOrDefault(); margin < 0 || margin > 1 {
		return fmt.Errorf("%smargin must be in [0, 1], got %v", prefix, margin)
	}
	if horizon := t.GuardHorizonOrDefault(); horizon < 0 {
		return fmt.Errorf("%sguard-horizon must not be negative, got %s", prefix, horizon)
	}
	if t.MaxDelay < 0 {
		return fmt.Errorf("%smax-delay must not be negative, got %s", prefix, t.MaxDelay)
	}
	return nil
}

// Warning describes a valid but risky setting, or returns "".
func (t PaceThrottleConfig) Warning() string {
	if t.MaxDelay >= throttleWarnDelay {
		return fmt.Sprintf("%sthrottle.max-delay %s is close to the 300 s that Claude Code, Codex and pi wait for a response; delayed requests may time out", routingPaceConfigPrefix, t.MaxDelay)
	}
	return ""
}

// MarginOrDefault returns Margin or its default.
func (t PaceThrottleConfig) MarginOrDefault() float64 {
	if t.Margin == nil {
		return DefaultThrottleMargin
	}
	return *t.Margin
}

// GuardHorizonOrDefault returns GuardHorizon or its default.
func (t PaceThrottleConfig) GuardHorizonOrDefault() time.Duration {
	if t.GuardHorizon == nil {
		return DefaultThrottleGuard
	}
	return *t.GuardHorizon
}

// BurstReserveOrDefault returns BurstReserve or its default.
func (p PaceRoutingConfig) BurstReserveOrDefault() float64 {
	if p.BurstReserve == nil {
		return DefaultPaceBurstReserve
	}
	return *p.BurstReserve
}

// PaceToleranceOrDefault returns PaceTolerance or its default.
func (p PaceRoutingConfig) PaceToleranceOrDefault() float64 {
	if p.PaceTolerance == nil {
		return DefaultPaceTolerance
	}
	return *p.PaceTolerance
}

// StaleAfterOrDefault returns StaleAfter, or its default when zero.
func (p PaceRoutingConfig) StaleAfterOrDefault() time.Duration {
	if p.StaleAfter == 0 {
		return DefaultPaceStaleAfter
	}
	return p.StaleAfter
}
