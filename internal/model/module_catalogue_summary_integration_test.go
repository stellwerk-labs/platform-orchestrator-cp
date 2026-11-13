//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestModuleCatalogueProposedSummaryIncludesAllVersionPages(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	org, targets, _ := externalOperationModules(t, db, uuid.New())
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE definition_versions SET created_at = now() - interval '1 day' WHERE org_id = $1`, org)
	require.NoError(t, err)
	// More than a page of newer historical versions hides the original Proposed
	// from the version-list endpoint, but not from the catalogue summary.
	_, err = tx.ExecContext(ctx, `INSERT INTO definition_versions
  (org_id, definition_id, module_uuid, version_id, created_at, module_source, module_inputs,
   dependencies, coprovisioned, provider_mapping, provider_values, semantic_status, migration_generation)
  SELECT $1, 'alpha', $2, 'history-' || n, now() - n * interval '1 second', 'inline',
   '{}', '{}', '[]', '{}', '{}', 'deprecated', 'v0'
  FROM generate_series(1, 101) n`, org, targets[0].ModuleUUID)
	require.NoError(t, err)
	_, err = db.CreateEmptyModule(ctx, tx, org, "empty", "Empty", "", "generic", map[string]string{})
	require.NoError(t, err)
	// Bravo retains its Default but has no Proposed versions.
	_, err = tx.ExecContext(ctx, `UPDATE definition_versions SET semantic_status = 'deprecated'
  WHERE org_id = $1 AND definition_id = 'bravo' AND semantic_status = 'proposed'`, org)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	versions, next, err := db.ListModuleDefinitionVersions(ctx, nil, org, "alpha", "", 100,
		ListModuleDefinitionVersionsParams{IncludeDeprecated: true, IncludeDefective: true})
	require.NoError(t, err)
	require.Len(t, versions, 100)
	require.NotEmpty(t, next)
	for _, version := range versions {
		require.NotEqual(t, targets[0].VersionUUID, version.VersionUUID)
	}

	entries, err := db.ListModuleCatalogues(ctx, nil, org, false)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, entry := range entries {
		require.NotNil(t, entry.DefaultVerified)
		require.False(t, *entry.DefaultVerified, "current schema only supports unverified versions")
		require.NotNil(t, entry.HasProposed)
		require.Equal(t, entry.Slug == "alpha", *entry.HasProposed, entry.Slug)
		if entry.Slug == "empty" {
			require.Nil(t, entry.CurrentDefaultVersionUUID)
		}
	}
	// Other catalogue reads do not claim to carry the list-only summary.
	entry, err := db.GetModuleCatalogue(ctx, nil, org, "alpha", GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, entry.HasProposed)
}
