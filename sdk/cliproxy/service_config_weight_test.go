package cliproxy

import (
	"time"

	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	if _, ok := newRoutingSelector(state, nil).(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.WeightedRoundRobinSelector", newRoutingSelector(state, nil))
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}

func TestPaceRoutingSelector(t *testing.T) {
	tolerance := 0.3
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: " Pace ", Pace: internalconfig.PaceRoutingConfig{PaceTolerance: &tolerance}},
	})
	want := coreauth.PaceConfig{
		BurstReserve:  internalconfig.DefaultPaceBurstReserve,
		PaceTolerance: 0.3,
		StaleAfter:    internalconfig.DefaultPaceStaleAfter,
		Throttle:      coreauth.ThrottleConfig{Threshold: internalconfig.DefaultThrottleThreshold, Floor: internalconfig.DefaultThrottleFloor},
	}
	if state.strategy != "pace" || state.pace != want {
		t.Fatalf("state = %+v, want pace with %+v", state, want)
	}
	if _, ok := newRoutingSelector(state, quota.NewCache()).(*coreauth.PaceSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.PaceSelector", newRoutingSelector(state, quota.NewCache()))
	}
	other := state
	other.pace.PaceTolerance = 0.2
	if state == other {
		t.Fatal("routing states with different pace settings must differ")
	}
	throttled := state
	throttled.pace.Throttle.MaxDelay = 2 * time.Minute
	if state == throttled {
		t.Fatal("routing states with different throttle settings must differ, so a reload applies them")
	}
}

func TestServiceRejectsInvalidPaceConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	reserve := 2.0
	newCfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "pace", Pace: internalconfig.PaceRoutingConfig{BurstReserve: &reserve}}}
	if commit := service.commitConfigUpdate(newCfg); commit.cfg != nil {
		t.Fatal("commitConfigUpdate accepted an invalid pace block")
	}
	if service.cfg != originalCfg {
		t.Fatal("rejected commit replaced the running config")
	}
}
