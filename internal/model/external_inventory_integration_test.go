//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExternalInventorySelectionAndVersionMetadata(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	environmentID := fixture.Intent.Environments[0].EnvironmentID
	modules := []string{fixture.Intent.Modules[0].String(), fixture.Intent.Modules[1].String()}
	snapshot, err := ReadExternalInventory(t.Context(), db, fixture.OrgID, modules, ExternalInventorySelector{EnvironmentIDs: []uuid.UUID{environmentID}, ProjectIDs: []string{"project"}, EnvironmentTypes: []string{"development"}})
	require.NoError(t, err)
	require.Len(t, snapshot.Environments, 1)
	require.Equal(t, fixture.Intent.Environments[0].ProjectID, snapshot.Environments[0].ProjectUuid)
	require.Len(t, snapshot.Modules, 2)
	require.Len(t, snapshot.Versions, 4)
	require.Len(t, snapshot.EnvironmentTypes, 1)
	versions, err := ListProposedCoreModuleVersions(t.Context(), db, fixture.OrgID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	for _, version := range versions {
		require.Equal(t, "1.1.0", *version.SemanticVersion)
		require.Equal(t, fixture.Intent.TargetVersions[version.ModuleUUID.String()], version.UUID)
	}
	empty, err := ReadExternalInventory(t.Context(), db, fixture.OrgID, nil, ExternalInventorySelector{Labels: map[string]string{"absent": ""}})
	require.NoError(t, err)
	require.Empty(t, empty.Environments, "missing label is not equivalent to present empty label")
	empty, err = ReadExternalInventory(t.Context(), db, fixture.OrgID, nil, ExternalInventorySelector{ProjectIDs: []string{"other"}})
	require.NoError(t, err)
	require.Empty(t, empty.Environments)
	_, err = ReadExternalInventory(t.Context(), db, fixture.OrgID, []string{modules[0], snapshot.Modules[0].Slug}, ExternalInventorySelector{})
	require.ErrorContains(t, err, "distinct identities")
	other := externalGrantFixture(t, db)
	_, err = ReadExternalInventory(t.Context(), db, other.OrgID, modules, ExternalInventorySelector{})
	require.Error(t, err, "Module identities are organization-scoped")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE definitions SET catalogue_status='archived', archived_at=now(), archived_by=$2, archive_reason='Retire fixture' WHERE uuid=$1`, fixture.Intent.Modules[0], fixture.ActorID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	versions, err = ListProposedCoreModuleVersions(t.Context(), db, fixture.OrgID)
	require.NoError(t, err)
	require.Len(t, versions, 1, "archived Modules must not be offered as new Proposed targets")
}
