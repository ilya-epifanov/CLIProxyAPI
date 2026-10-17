package config

import (
	"fmt"
	"time"
)

// Defaults of the "pace" routing strategy.
const (
	DefaultPaceBurstReserve  = 0.2
	DefaultPaceTolerance     = 0.1
	DefaultPaceStaleAfter    = 15 * time.Minute
	DefaultThrottleThreshold = 0.8
	DefaultThrottleFloor     = 0.05
	routingPaceConfigPrefix  = "routing.pace."
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
	// Throttle slows every request down while the pool spends quota ahead of schedule.
	Throttle PaceThrottleConfig `yaml:"throttle,omitempty" json:"throttle,omitempty"`
}

// PaceThrottleConfig slows requests down while the pool's pace (remaining quota over
// time left, summed over the usable accounts, lowest over the model's windows) is below
// Threshold: each request waits MaxDelay × (Threshold − pace) / (Threshold − Floor),
// capped at MaxDelay. A MaxDelay of 0 turns it off.
type PaceThrottleConfig struct {
	// Threshold is the pool pace below which requests are delayed; (Floor, 1].
	Threshold *float64 `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// Floor is the pool pace at and below which requests wait MaxDelay; [0, Threshold).
	Floor *float64 `yaml:"floor,omitempty" json:"floor,omitempty"`
	// MaxDelay is the longest wait added to one request. 0 (default) turns throttling off.
	MaxDelay time.Duration `yaml:"max-delay,omitempty" json:"max-delay,omitempty"`
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
	threshold, floor := t.ThresholdOrDefault(), t.FloorOrDefault()
	if threshold <= 0 || threshold > 1 {
		return fmt.Errorf("%sthreshold must be in (0, 1], got %v", prefix, threshold)
	}
	if floor < 0 || floor >= threshold {
		return fmt.Errorf("%sfloor must be in [0, threshold %v), got %v", prefix, threshold, floor)
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

// ThresholdOrDefault returns Threshold or its default.
func (t PaceThrottleConfig) ThresholdOrDefault() float64 {
	if t.Threshold == nil {
		return DefaultThrottleThreshold
	}
	return *t.Threshold
}

// FloorOrDefault returns Floor or its default.
func (t PaceThrottleConfig) FloorOrDefault() float64 {
	if t.Floor == nil {
		return DefaultThrottleFloor
	}
	return *t.Floor
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
