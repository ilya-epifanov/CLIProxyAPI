package auth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
	log "github.com/sirupsen/logrus"
)

// Drain: the operator flags accounts whose quota should be used up before a reset they
// plan to spend. Under pace, while a flagged account can serve a request, the pick
// considers the flagged accounts only, whatever their priority, the one closest to its
// limit first, and the pace throttle adds no delay. A flag clears itself once an
// observation shows the account's longest window used up.

// DrainFlag marks one account for draining.
type DrainFlag struct {
	SetAt time.Time `json:"set_at"`
}

// drainFlagsFile is the stored format; Version changes when the format does.
type drainFlagsFile struct {
	Version int                  `json:"version"`
	Flags   map[string]DrainFlag `json:"flags"`
}

const drainFlagsVersion = 1

// DrainFlags holds the drain flags by auth ID. With a path, every change is written
// there and the flags survive a restart. A nil *DrainFlags has no flags.
type DrainFlags struct {
	mu    sync.RWMutex
	path  string
	flags map[string]DrainFlag
}

// NewDrainFlags returns flags kept in memory only.
func NewDrainFlags() *DrainFlags {
	return &DrainFlags{flags: map[string]DrainFlag{}}
}

// LoadDrainFlags reads the flags stored at path; a missing file has none.
func LoadDrainFlags(path string) (*DrainFlags, error) {
	out := &DrainFlags{path: path, flags: map[string]DrainFlag{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read drain flags: %w", err)
	}
	var file drainFlagsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse drain flags %s: %w", path, err)
	}
	if file.Version != drainFlagsVersion {
		return nil, fmt.Errorf("drain flags %s: version %d, want %d", path, file.Version, drainFlagsVersion)
	}
	maps.Copy(out.flags, file.Flags)
	return out, nil
}

// Has reports whether authID is flagged.
func (d *DrainFlags) Has(authID string) bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.flags[authID]
	return ok
}

// empty reports whether no account is flagged.
func (d *DrainFlags) empty() bool {
	if d == nil {
		return true
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.flags) == 0
}

// Set flags authID for draining, or clears its flag. The flags are stored before they
// take effect, so a failed write changes nothing.
func (d *DrainFlags) Set(authID string, drain bool, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, flagged := d.flags[authID]; flagged == drain {
		return nil
	}
	next := maps.Clone(d.flags)
	if drain {
		next[authID] = DrainFlag{SetAt: now.UTC()}
	} else {
		delete(next, authID)
	}
	if err := d.storeLocked(next); err != nil {
		return err
	}
	d.flags = next
	return nil
}

// storeLocked replaces the file with flags, so a crash leaves the old or the new one.
func (d *DrainFlags) storeLocked(flags map[string]DrainFlag) error {
	if d.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(drainFlagsFile{Version: drainFlagsVersion, Flags: flags}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode drain flags: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".drain-flags-*.json")
	if err != nil {
		return fmt.Errorf("store drain flags: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store drain flags: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store drain flags: %w", err)
	}
	if err := os.Rename(tmp.Name(), d.path); err != nil {
		return fmt.Errorf("store drain flags: %w", err)
	}
	return nil
}

// DrainFlags returns the accounts flagged for draining.
func (m *Manager) DrainFlags() *DrainFlags {
	return m.drains
}

// SetDrainFlags installs stored flags; the service calls it before it serves requests.
func (m *Manager) SetDrainFlags(flags *DrainFlags) {
	m.drains = flags
}

// ClearSpentDrain clears authID's drain flag when its quota reads and response headers
// show its longest routing window used up: the reset the operator planned to spend is
// the next one. The pace selector's quota view is the only one the manager reads, so it
// does nothing under other strategies.
func (m *Manager) ClearSpentDrain(authID string) {
	if !m.drains.Has(authID) {
		return
	}
	now := time.Now()
	m.mu.RLock()
	var windows quota.AccountWindows
	observed := false
	if pace, ok := paceSelectorOf(m.selector); ok {
		if auth := m.auths[authID]; auth != nil {
			// The credential's windows, without a model-scoped one.
			windows, observed = pace.windowsOf(auth, "")
		}
	}
	m.mu.RUnlock()
	if !observed || !longestSpent(windows, now) {
		return
	}
	if err := m.drains.Set(authID, false, now); err != nil {
		log.WithError(err).Error("drain: store flags")
		return
	}
	log.WithField("auth_id", authID).Info("drain: the account's longest window is used up; cleared its drain flag")
}

// drainingLocked returns the IDs among auths that are flagged with no routing window
// used up; nil unless the strategy is pace. The caller holds m.mu.
func (m *Manager) drainingLocked(auths []*Auth, routeModel string, now time.Time) map[string]struct{} {
	pace, ok := paceSelectorOf(m.selector)
	if !ok || m.drains.empty() {
		return nil
	}
	var out map[string]struct{}
	for _, auth := range auths {
		if m.drains.Has(auth.ID) && pace.classify(auth, m.selectionModelForAuth(auth, routeModel), true, now).draining {
			if out == nil {
				out = map[string]struct{}{}
			}
			out[auth.ID] = struct{}{}
		}
	}
	return out
}

// drainServesLocked reports whether a flagged account can serve model for one of
// providers, in any priority tier. The caller holds m.mu.
func (m *Manager) drainServesLocked(providers []string, model string, now time.Time) bool {
	if m.drains.empty() {
		return false
	}
	var auths []*Auth
	for _, provider := range providers {
		for _, account := range m.rankingPoolLocked(provider, model, now) {
			if !account.blocked {
				auths = append(auths, account.auth)
			}
		}
	}
	return len(m.drainingLocked(auths, model, now)) > 0
}

type drainingKey struct{}

// withDraining attaches the draining accounts (drainingLocked) of a pick or a ranking.
func withDraining(ctx context.Context, ids map[string]struct{}) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, drainingKey{}, ids)
}

func drainingFrom(ctx context.Context) map[string]struct{} {
	if ctx == nil {
		return nil
	}
	ids, _ := ctx.Value(drainingKey{}).(map[string]struct{})
	return ids
}

// drainingOnly narrows auths to the draining ones when any is among them: drain is a
// priority level above every configured one. It runs before the tier narrowing.
func drainingOnly(ctx context.Context, auths []*Auth) []*Auth {
	ids := drainingFrom(ctx)
	var out []*Auth
	for _, auth := range auths {
		if _, ok := ids[auth.ID]; ok {
			out = append(out, auth)
		}
	}
	if len(out) == 0 {
		return auths
	}
	return out
}

// longestWindows returns the windows of the greatest length among windows.
func longestWindows(windows []quota.Window) []quota.Window {
	var out []quota.Window
	for _, w := range windows {
		switch {
		case len(out) == 0 || w.Length > out[0].Length:
			out = append(out[:0], w)
		case w.Length == out[0].Length:
			out = append(out, w)
		}
	}
	return out
}

// longestSpent reports whether a longest window of w is used up. While a long slot is
// unobserved the longest window is unknown, so it reports false.
func longestSpent(w quota.AccountWindows, now time.Time) bool {
	if !w.Complete {
		return false
	}
	for _, lw := range longestWindows(w.All()) {
		if exhausted(lw, now) {
			return true
		}
	}
	return false
}

// longestLeft is the remaining fraction of the longest windows of w, the lowest when
// several share that length; a reset that has passed counts as refilled.
func longestLeft(w quota.AccountWindows, now time.Time) float64 {
	left := math.Inf(1)
	for _, lw := range longestWindows(w.All()) {
		remaining, _ := windowShare(lw, now)
		left = min(left, remaining)
	}
	return left
}
