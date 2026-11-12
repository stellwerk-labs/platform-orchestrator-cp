//go:build integration

package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInstallationMaintenanceFencesPluginEffectsAndInventory(t *testing.T) {
	db := externalOperationDatabase(t)
	require.NoError(t, ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 1}))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, RequirePluginMaintenanceAdmission(t.Context(), tx, "module.promote"))
	completed := make(chan error, 1)
	go func() {
		completed <- ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 2, Enabled: true})
	}()
	select {
	case err := <-completed:
		t.Fatalf("barrier passed active effect: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("barrier did not finish")
	}
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, action := range []string{"external-deployment.admit", "external-deployment.rollback", "module.promote", "module.restore", "pin.override.begin", "observation.try-start", "operation.activate"} {
		require.ErrorIs(t, RequirePluginMaintenanceAdmission(t.Context(), tx, action), ErrInstallationMaintenance, action)
	}
	for _, action := range []string{"external-deployment.read", "external-deployment.withdraw", "pin.override.reconcile", "pin.override.restore", "pin.override.cancel", "observation.get", "observation.request.resolve", "operation.release"} {
		require.NoError(t, RequirePluginMaintenanceAdmission(t.Context(), tx, action), action)
	}
	_, err = LockPluginManager(t.Context(), tx)
	require.ErrorContains(t, err, "paused during maintenance")
	require.NoError(t, tx.Rollback())
	require.NoError(t, ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 3}))
	require.Error(t, ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 2, Enabled: true}))
	require.Error(t, ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 3, Enabled: true}))
	current, err := ReadInstallationMaintenance(t.Context(), db)
	require.NoError(t, err)
	require.False(t, current.Enabled)
}

func TestUnchangedMaintenanceDoesNotQueueBehindActiveEffect(t *testing.T) {
	db := externalOperationDatabase(t)
	state := InstallationMaintenance{Revision: 1}
	require.NoError(t, ApplyInstallationMaintenance(t.Context(), db, state))
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, RequirePluginMaintenanceAdmission(t.Context(), tx, "module.promote"))
	result := make(chan error, 1)
	go func() { result <- ApplyInstallationMaintenance(t.Context(), db, state) }()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("unchanged maintenance blocked on an active reader")
	}
}

func TestMaintenanceRefusesOtherAndUnclassifiedManagerCommands(t *testing.T) {
	db := externalOperationDatabase(t)
	require.NoError(t, ApplyInstallationMaintenance(t.Context(), db, InstallationMaintenance{Revision: 2, Enabled: true}))
	for _, action := range []string{"", "unknown", "plugin_installation.create", "plugin_installation.identity",
		"plugin_installation.enable", "plugin_installation.disable", "plugin_installation.uninstall", "plugin_installation.reinstall",
		"plugin_installation.purge", "plugin_enablement.put", "plugin_enablement.remove", "plugin_entitlement_bundle.import"} {
		t.Run(action, func(t *testing.T) {
			mutated := false
			fingerprint := action + ":sha256:" + strings.Repeat("a", 64)
			if action == "" {
				fingerprint = "plugin_installation.activate:sha256:" + strings.Repeat("a", 64)
			}
			_, err := ExecutePluginManagerCommand(t.Context(), db, PluginManagerCommand{
				Actor: fixtureOperator, IdempotencyKey: "refuse-" + action, Action: action,
				Fingerprint: fingerprint, ExpectedRevision: pluginManagerRevision(t, db), Reason: "Try a paused command",
			}, func(context.Context, Tx) (*PluginManagerChange, error) {
				mutated = true
				return nil, nil
			}, func(context.Context, Tx, int64) (any, error) { return nil, nil })
			requireConflictCode(t, err, "system_maintenance")
			require.False(t, mutated)
		})
	}
	require.Zero(t, pluginManagerRevision(t, db))
	require.Zero(t, pluginManagerAuditCount(t, db))
}
