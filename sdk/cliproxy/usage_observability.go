package cliproxy

import (
	"context"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagemetrics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	usageStorePluginName   = "usage-store"
	usageMetricsPluginName = "usage-metrics"
	paceLedgerPluginName   = "pace-ledger"
	usageStoreFileName     = "usage.db"
	// paceLedgerSeedSpan is how much stored history the pace throttle's ledger reads at start.
	paceLedgerSeedSpan = 7 * 24 * time.Hour
)

// startUsageObservability registers the metrics aggregator, opens the usage
// store when it is enabled, and builds the metrics server. An enabled store
// that cannot be opened is a configuration error, so the service does not start.
func (s *Service) startUsageObservability(ctx context.Context) error {
	s.usageMetrics = usagemetrics.NewAggregator()
	usage.RegisterNamedPlugin(usageMetricsPluginName, s.usageMetrics)

	// A nil *usagestore.Store must not become a non-nil StoreStats.
	var storeStats usagemetrics.StoreStats
	if s.cfg != nil && s.cfg.UsageStore.Enable {
		path := usageStorePath(s.cfg.UsageStore)
		store, err := usagestore.Open(ctx, usagestore.Options{Path: path, Retention: usageStoreRetention(s.cfg.UsageStore)})
		if err != nil {
			return err
		}
		s.usageStore = store
		s.usageStorePath = path
		usage.RegisterNamedPlugin(usageStorePluginName, store)
		storeStats = store
		log.Infof("usage store opened at %s", path)
	}
	if s.coreManager != nil {
		// Seeded before it is registered, so no request is counted twice.
		if s.usageStore != nil {
			seedPaceLedger(ctx, s.usageStore, s.coreManager.Ledger())
		}
		usage.RegisterNamedPlugin(paceLedgerPluginName, s.coreManager.Ledger())
	}

	var auths usagemetrics.AuthLister
	if s.coreManager != nil {
		auths = s.coreManager.List
	}
	s.metricsServer = newMetricsServer(usagemetrics.NewHandler(s.usageMetrics, auths, storeStats))
	return nil
}

// seedPaceLedger gives the pace throttle the last week of stored usage, so a restart
// does not cost it its forecast and quota sizes. A failed read only leaves it to learn.
func seedPaceLedger(ctx context.Context, store *usagestore.Store, ledger *coreauth.UsageLedger) {
	var entries []coreauth.LedgerEntry
	covered, err := store.RequestsSince(ctx, time.Now().Add(-paceLedgerSeedSpan), func(r usagestore.RequestUsage) {
		t := r.Tokens
		entries = append(entries, coreauth.LedgerEntry{
			At:        r.At,
			Provider:  r.Provider,
			AuthIndex: r.AuthIndex,
			Model:     r.Model,
			SessionID: r.SessionID,
			Units:     coreauth.CostUnitsOf(t.Input, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified),
		})
	})
	if err != nil {
		log.Warnf("pace throttle: read stored usage: %v; it learns quota sizes from new requests", err)
		return
	}
	ledger.Seed(covered, entries)
	log.Infof("pace throttle: read %d stored requests since %s", len(entries), covered.UTC().Format(time.RFC3339))
}

// usageStorePath resolves an empty path to usage.db in the writable path, or
// in the working directory when no writable path is set.
func usageStorePath(cfg config.UsageStoreConfig) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	if dir := util.WritablePath(); dir != "" {
		return filepath.Join(dir, usageStoreFileName)
	}
	return usageStoreFileName
}

func usageStoreRetention(cfg config.UsageStoreConfig) usagestore.Retention {
	return usagestore.Retention{RequestDays: cfg.RetentionDays, RollupDays: cfg.RollupRetentionDays}
}

// applyUsageStoreConfig applies a reloaded retention. Enabling, disabling or
// moving the store takes effect only at the next start.
func (s *Service) applyUsageStoreConfig(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	opened := s.usageStore != nil
	if cfg.UsageStore.Enable != opened || (opened && usageStorePath(cfg.UsageStore) != s.usageStorePath) {
		log.Warn("observability.usage.store enable or path changed; restart to apply")
	}
	if !opened {
		return
	}
	if err := s.usageStore.SetRetention(usageStoreRetention(cfg.UsageStore)); err != nil {
		log.WithError(err).Error("usage store: keeping the previous retention")
	}
}

// closeUsageStore stores the records still pending in the usage store. Call
// it after the usage manager has delivered its queue.
func (s *Service) closeUsageStore(ctx context.Context) error {
	if s == nil || s.usageStore == nil {
		return nil
	}
	return s.usageStore.Close(ctx)
}
