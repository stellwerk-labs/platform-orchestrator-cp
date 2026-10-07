package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
)

// Plugin maintenance runs in every replica. Each job is safe to run
// concurrently and request decisions never wait for it.
const (
	entitlementSweepInterval = 15 * time.Second
	entitlementSweepTimeout  = 10 * time.Second
	journalRetentionInterval = 10 * time.Minute
	journalRetentionTimeout  = 5 * time.Minute
	journalRetentionBatch    = 10000
)

// runEvery calls run immediately and then once per interval until ctx ends.
// Each call gets its own bounded context; a failure is logged and the job is
// retried on the next tick.
func runEvery(ctx context.Context, interval, timeout time.Duration, logger *zap.Logger, job string, run func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		err := run(bounded)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Error("Plugin maintenance job failed", zap.String("job", job), zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepPluginEntitlements records elapsed licence boundaries even when there
// is no user traffic, without calling a plugin.
func sweepPluginEntitlements(adapter *pluginentitlements.Adapter) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := adapter.Sweep(ctx, 1000)
		return err
	}
}

// pruneExtensionJournal applies the explicit journal retention policy for the
// subscriptions recorded by the Plugin Manager.
func pruneExtensionJournal(db model.Databaser, policy model.ExtensionResourceRetention, logger *zap.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		pruned, err := model.PruneExtensionResourceJournal(ctx, db, policy, time.Now())
		if pruned > 0 {
			logger.Info("Pruned extension resource journal entries", zap.Int64("entries", pruned))
		}
		return err
	}
}

// operatorIDs parses configured permission holders; the configuration
// validates them as UUIDs.
func operatorIDs(values []string) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		ids = append(ids, uuid.MustParse(value))
	}
	return ids
}
