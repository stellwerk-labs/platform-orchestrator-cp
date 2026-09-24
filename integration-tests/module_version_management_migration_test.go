package integrationtests

import (
	"github.com/google/uuid"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModuleVersionManagementMigrationRoundTrip(t *testing.T) {
	database := MustDatabaser(t)
	migration, err := os.ReadFile("../internal/model/migrations/000024_module_version_management_core.sql")
	require.NoError(t, err)
	up, down := migrationSections(t, string(migration))
	extensionMigration, err := os.ReadFile("../internal/model/migrations/000025_module_extension_contract.sql")
	require.NoError(t, err)
	extensionUp, extensionDown := migrationSections(t, string(extensionMigration))

	tx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	if columnExists(t, tx, "definitions", "uuid") {
		_, err = tx.ExecContext(t.Context(), extensionDown)
		require.NoError(t, err, "the extension recovery migration must release its references first")
		_, err = tx.ExecContext(t.Context(), down)
		require.NoError(t, err, "the recovery migration must restore the previous schema")
	}
	assert.False(t, relationExists(t, tx, "module_version_lifecycle_events"))
	assert.False(t, relationExists(t, tx, "environment_module_version_pins"))
	assert.False(t, columnExists(t, tx, "definitions", "uuid"))
	assert.False(t, columnExists(t, tx, "definition_versions", "semantic_version"))
	assert.False(t, columnExists(t, tx, "definition_versions", "artifact_digest"))
	assert.False(t, columnExists(t, tx, "envs", "labels"))
	assert.False(t, columnExists(t, tx, "env_types", "is_production"))

	legacyOrg := "upgrade-" + uuid.NewString()
	_, err = tx.ExecContext(t.Context(), `INSERT INTO orgs(id, created_at) VALUES ($1, now())`, legacyOrg)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO definitions(org_id, id, created_at, resource_type, latest_version_id)
		VALUES ($1, 'legacy-service', now(), 'test-service', 'old-default')`, legacyOrg)
	require.NoError(t, err)
	for _, version := range []string{"old-deprecated", "old-default"} {
		_, err = tx.ExecContext(t.Context(), `INSERT INTO definition_versions
			(org_id, definition_id, version_id, created_at, module_source, module_inputs, dependencies, coprovisioned, provider_mapping, provider_values)
			VALUES ($1, 'legacy-service', $2, now(), 'git::https://example.com/service.git?ref=old', '{"replicas":2}', '{}', '[]', '{}', '{}')`, legacyOrg, version)
		require.NoError(t, err)
	}
	// The upgrade starts from committed legacy data. Flush its deferred checks
	// while keeping this schema round trip isolated inside the test transaction.
	_, err = tx.ExecContext(t.Context(), `SET CONSTRAINTS ALL IMMEDIATE`)
	require.NoError(t, err)

	_, err = tx.ExecContext(t.Context(), up)
	require.NoError(t, err, "the Core migration must reapply after recovery")
	assert.True(t, relationExists(t, tx, "module_version_lifecycle_events"))
	assert.True(t, relationExists(t, tx, "environment_module_version_pins"))
	assert.True(t, columnExists(t, tx, "definitions", "uuid"))
	assert.True(t, columnExists(t, tx, "definition_versions", "semantic_version"))
	assert.True(t, columnExists(t, tx, "definition_versions", "artifact_digest"))
	assert.True(t, columnExists(t, tx, "envs", "labels"))
	assert.True(t, columnExists(t, tx, "env_types", "is_production"))

	var invalidLegacyRows int
	require.NoError(t, tx.QueryRowContext(t.Context(), `
		SELECT count(*) FROM definition_versions
		WHERE migration_generation <> 'v0' OR semantic_version IS NOT NULL
	`).Scan(&invalidLegacyRows))
	assert.Zero(t, invalidLegacyRows, "migration must not invent SemVer metadata for legacy versions")
	var preservedVersions int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM definition_versions
		WHERE org_id = $1 AND migration_generation = 'v0' AND semantic_version IS NULL AND artifact_digest IS NULL
		AND module_inputs = '{"replicas":2}'::jsonb AND module_source = 'git::https://example.com/service.git?ref=old'
		AND ((version_id = 'old-default' AND semantic_status = 'default')
		OR (version_id = 'old-deprecated' AND semantic_status = 'deprecated'))`, legacyOrg).Scan(&preservedVersions))
	assert.Equal(t, 2, preservedVersions, "all historical definitions and the exact old Default must survive migration")
	var defaultIdentity string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT v.version_id FROM definitions d
		JOIN definition_versions v ON v.module_uuid = d.uuid AND v.uuid = d.current_default_version_uuid
		WHERE d.org_id = $1 AND d.id = 'legacy-service' AND d.latest_version_id = v.version_id`, legacyOrg).Scan(&defaultIdentity))
	assert.Equal(t, "old-default", defaultIdentity)

	_, err = tx.ExecContext(t.Context(), extensionUp)
	require.NoError(t, err, "the extension contract must reapply after recovery")
	assert.True(t, relationExists(t, tx, "module_operation_reservations"))
	assert.True(t, relationExists(t, tx, "module_extension_contributions"))
	assert.True(t, columnExists(t, tx, "module_extension_contributions", "environment_uuid"))
	require.NoError(t, tx.Rollback(), "round-trip verification must leave the shared integration database unchanged")
}
