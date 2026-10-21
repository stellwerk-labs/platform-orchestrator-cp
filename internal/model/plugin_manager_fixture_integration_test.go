//go:build integration

package model

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fixturePlugin is the plugin of every external operation fixture.
const fixturePlugin = "dev.example.workflow"

var fixtureOperator = uuid.MustParse("0199a0c1-1d6e-72f1-8000-0000000000aa")

// subscribeToResourceEvents enables plugin for org with a registered resource
// event read grant, installing and activating the plugin first when needed.
// It writes the model state directly, as the Plugin Manager would.
func subscribeToResourceEvents(t *testing.T, db Databaser, org, plugin string) PluginInstallation {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	installation, err := fixtureInstallation(t, tx, plugin)
	require.NoError(t, err)
	require.NoError(t, PutPluginEnablement(ctx, tx, installation, PluginEnablement{OrgID: org, State: PluginEnablementEnabled, Timezone: "UTC", ApprovalPolicies: map[string]string{}}, fixtureOperator))
	require.NoError(t, tx.Commit())
	return installation
}

// unsubscribeFromResourceEvents removes the plugin's enablement for org.
func unsubscribeFromResourceEvents(t *testing.T, db Databaser, org, plugin string) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	deleted, err := DeletePluginEnablement(t.Context(), tx, plugin, org)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoError(t, tx.Commit())
}

// journalFixtureOrganization subscribes the first fixture organization of a
// schema. In v1alpha an installation serves one organization, so later
// fixture organizations in the same schema stay unsubscribed.
func journalFixtureOrganization(t *testing.T, db Databaser, org string) {
	t.Helper()
	var enabled bool
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM plugin_enablements WHERE plugin_id=$1)`, fixturePlugin).Scan(&enabled))
	if !enabled {
		subscribeToResourceEvents(t, db, org, fixturePlugin)
	}
}

func fixtureInstallation(t *testing.T, tx Tx, plugin string) (PluginInstallation, error) {
	t.Helper()
	ctx := t.Context()
	var id uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT installation_uuid FROM plugin_installations WHERE plugin_id=$1 AND state<>'uninstalled'`, plugin).Scan(&id)
	if err == nil {
		return GetPluginInstallation(ctx, tx, id, true)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PluginInstallation{}, err
	}
	grant, err := json.Marshal(map[string]string{"action": extensionResourceEventsRead, "permission": "plugin." + plugin + ".events", "scope": "organization"})
	require.NoError(t, err)
	manifest, err := json.Marshal(map[string]any{"id": plugin, "display_name": plugin, "version": "1.0.0", "service_grants": []json.RawMessage{grant}})
	require.NoError(t, err)
	digest := sha256.Sum256(manifest)
	pkg := PluginPackage{ManifestSHA256: hex.EncodeToString(digest[:]), PluginID: plugin, Version: "1.0.0", Manifest: manifest, CreatedBy: fixtureOperator}
	if err := InsertPluginPackage(ctx, tx, pkg); err != nil {
		return PluginInstallation{}, err
	}
	installation := PluginInstallation{InstallationID: uuid.New(), PluginID: plugin, PackageSHA256: pkg.ManifestSHA256, State: PluginInstallationActive, BackendURL: "https://" + plugin + ".test", CreatedBy: fixtureOperator}
	if err := InsertPluginInstallation(ctx, tx, installation); err != nil {
		return PluginInstallation{}, err
	}
	if err := ReplacePluginRegistrations(ctx, tx, installation, []PluginRegistration{{Kind: PluginRegistrationServiceGrant, Key: extensionResourceEventsRead, Definition: grant}}); err != nil {
		return PluginInstallation{}, err
	}
	return GetPluginInstallation(ctx, tx, installation.InstallationID, true)
}
