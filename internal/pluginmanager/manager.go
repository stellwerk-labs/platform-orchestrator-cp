// Package pluginmanager is the database-backed Plugin Manager: the only source
// of plugin installation truth. Operators change it through idempotent,
// audited commands; every Control Plane replica serves read decisions from a
// snapshot it refreshes when the revision moves.
package pluginmanager

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// Config wires a Manager.
type Config struct {
	Database     model.Databaser
	Registry     *plugins.InstallationRegistry
	Entitlements *pluginentitlements.Adapter
	// StatusClient reads plugin status endpoints. It carries the operator's
	// plugin CA, like the client that calls plugin operations.
	StatusClient *http.Client
	// AllowDevelopmentHTTP permits HTTP plugin origins in development only.
	AllowDevelopmentHTTP bool
	Logger               *zap.Logger
}

// Manager executes operator commands and refreshes this replica's snapshot.
type Manager struct {
	db                   model.Databaser
	registry             *plugins.InstallationRegistry
	entitlements         *pluginentitlements.Adapter
	statusClient         *http.Client
	allowDevelopmentHTTP bool
	logger               *zap.Logger
	refresh              sync.Mutex
}

// New validates the configuration.
func New(config Config) (*Manager, error) {
	if config.Database == nil || config.Registry == nil || config.Entitlements == nil || config.StatusClient == nil {
		return nil, errors.New("the Plugin Manager needs a database, a registry, the entitlement adapter and a plugin status client")
	}
	logger := config.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Manager{db: config.Database, registry: config.Registry, entitlements: config.Entitlements, statusClient: config.StatusClient, allowDevelopmentHTTP: config.AllowDevelopmentHTTP, logger: logger}, nil
}

// Refresh publishes the current Plugin Manager state to the registry when its
// revision moved. An installation that fails validation is not served and is
// logged; it never stops the others.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refresh.Lock()
	defer m.refresh.Unlock()
	var revision int64
	if err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		revision, err = model.PluginManagerRevision(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	if revision == m.registry.Revision() {
		return nil
	}
	snapshot, err := model.LoadPluginManagerSnapshot(ctx, m.db)
	if err != nil {
		return err
	}
	items, rejected := servedInstallations(snapshot)
	rejected = append(rejected, m.registry.Replace(snapshot.Revision, items, m.allowDevelopmentHTTP)...)
	for _, err := range rejected {
		m.logger.Error("Plugin installation is not served", zap.Int64("revision", snapshot.Revision), zap.Error(err))
	}
	return nil
}

// Run refreshes the snapshot every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Refresh(ctx); err != nil && ctx.Err() == nil {
				m.logger.Error("Plugin Manager snapshot refresh failed", zap.Error(err))
			}
		}
	}
}

// readOnly runs read in one repeatable-read snapshot.
func readOnly(ctx context.Context, db model.Databaser, read func(model.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit()
}
