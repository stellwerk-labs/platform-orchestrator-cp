package main

import (
	"context"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"go.uber.org/zap"
)

// Request decisions remain authoritative. This sweep records elapsed licence
// boundaries even when there is no user traffic, without calling a plugin.
func sweepPluginEntitlements(ctx context.Context, adapter *pluginentitlements.Adapter, logger *zap.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := adapter.Sweep(bounded, 1000)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Error("Plugin entitlement boundary sweep failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
