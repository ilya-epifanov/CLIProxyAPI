package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ThrottleOverride replaces a pool's computed throttle speed until it is cleared, for
// when the operator knows more than the plan: banked resets the plan does not count,
// or a deadline worth running the pool dry for.
type ThrottleOverride struct {
	// Speed is the share of full speed requests run at, in [0, 1]; 1 never delays.
	Speed float64   `json:"speed"`
	SetAt time.Time `json:"set_at"`
}

// throttleOverridesFile is the stored format; Version changes when the format does.
type throttleOverridesFile struct {
	Version   int                         `json:"version"`
	Overrides map[string]ThrottleOverride `json:"overrides"`
}

const throttleOverridesVersion = 1

// ThrottleOverrides holds the override per provider pool. With a path, every change is
// written there and the overrides survive a restart.
type ThrottleOverrides struct {
	mu        sync.RWMutex
	path      string
	overrides map[string]ThrottleOverride
}

// NewThrottleOverrides returns overrides kept in memory only.
func NewThrottleOverrides() *ThrottleOverrides {
	return &ThrottleOverrides{overrides: map[string]ThrottleOverride{}}
}

// LoadThrottleOverrides reads the overrides stored at path; a missing file has none.
func LoadThrottleOverrides(path string) (*ThrottleOverrides, error) {
	out := &ThrottleOverrides{path: path, overrides: map[string]ThrottleOverride{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read throttle overrides: %w", err)
	}
	var file throttleOverridesFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse throttle overrides %s: %w", path, err)
	}
	if file.Version != throttleOverridesVersion {
		return nil, fmt.Errorf("throttle overrides %s: version %d, want %d", path, file.Version, throttleOverridesVersion)
	}
	for provider, o := range file.Overrides {
		if err := ValidateThrottleSpeed(o.Speed); err != nil {
			return nil, fmt.Errorf("throttle overrides %s: %s: %w", path, provider, err)
		}
		out.overrides[provider] = o
	}
	return out, nil
}

// ValidateThrottleSpeed checks that speed is a share of full speed.
func ValidateThrottleSpeed(speed float64) error {
	if !(speed >= 0 && speed <= 1) {
		return fmt.Errorf("speed %v is not in [0, 1]", speed)
	}
	return nil
}

// Get returns provider's override, if one is set.
func (o *ThrottleOverrides) Get(provider string) (ThrottleOverride, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	v, ok := o.overrides[provider]
	return v, ok
}

// Set overrides provider's speed, which ValidateThrottleSpeed has accepted.
func (o *ThrottleOverrides) Set(provider string, speed float64, now time.Time) (ThrottleOverride, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	next := maps.Clone(o.overrides)
	next[provider] = ThrottleOverride{Speed: speed, SetAt: now.UTC()}
	if err := o.storeLocked(next); err != nil {
		return ThrottleOverride{}, err
	}
	o.overrides = next
	return next[provider], nil
}

// Clear returns provider to the computed throttle.
func (o *ThrottleOverrides) Clear(provider string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.overrides[provider]; !ok {
		return nil
	}
	next := maps.Clone(o.overrides)
	delete(next, provider)
	if err := o.storeLocked(next); err != nil {
		return err
	}
	o.overrides = next
	return nil
}

// storeLocked replaces the file with overrides, so a crash leaves the old or the new one.
func (o *ThrottleOverrides) storeLocked(overrides map[string]ThrottleOverride) error {
	if o.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(throttleOverridesFile{Version: throttleOverridesVersion, Overrides: overrides}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode throttle overrides: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".throttle-overrides-*.json")
	if err != nil {
		return fmt.Errorf("store throttle overrides: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store throttle overrides: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store throttle overrides: %w", err)
	}
	if err := os.Rename(tmp.Name(), o.path); err != nil {
		return fmt.Errorf("store throttle overrides: %w", err)
	}
	return nil
}
