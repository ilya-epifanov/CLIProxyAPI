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
	routingPaceConfigPrefix = "routing.pace."
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
	return nil
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
