//go:build integration

package model

import (
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stretchr/testify/require"
)

func archivedOperationTargets(t *testing.T, db Databaser, grant *ExternalOperationGrant) []ExternalModuleTarget {
	t.Helper()
	targets := make([]ExternalModuleTarget, 0, len(grant.Intent.Modules))
	for _, moduleID := range grant.Intent.Modules {
		versionID := grant.Intent.TargetVersions[moduleID.String()]
		version, err := db.GetCoreModuleVersion(t.Context(), nil, grant.OrgID, moduleID.String(), versionID.String(), GetModeDefault)
		require.NoError(t, err)
		baseline := grant.BaselineDefaults[moduleID.String()]
		require.NotNil(t, baseline)
		targets = append(targets, ExternalModuleTarget{
			ModuleUUID: moduleID, VersionUUID: version.UUID, ExpectedResourceVersion: version.ResourceVersion,
			ExpectedDefaultUUID: baseline,
		})
	}
	return targets
}

func TestArchiveAndPrepareRespectNativeOperationReservations(t *testing.T) {
	t.Run("archive is blocked while an active operation owns the module reservation", func(t *testing.T) {
		db := externalOperationDatabase(t)
		prepare := externalGrantFixture(t, db)
		prepare.Intent.Actions = append(prepare.Intent.Actions, "module.reserve", "module.release")
		grant, err := ApplyExternalGrantCommand(t.Context(), db, prepare)
		require.NoError(t, err)
		activate := prepare
		activate.Intent = nil
		activate.Action, activate.RequestID, activate.ExpectedRevision = "activate", "activate", grant.Revision
		grant, err = ApplyExternalGrantCommand(t.Context(), db, activate)
		require.NoError(t, err)

		moduleID := grant.Intent.Modules[0]
		module, err := db.GetModuleCatalogue(t.Context(), nil, grant.OrgID, moduleID.String(), GetModeDefault)
		require.NoError(t, err)
		reservation := ExternalModuleCommand{
			OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID,
			RequestID: "reserve-module", Action: "reserve", GrantFence: grant.Revision, Reason: "Retain active operation reservation",
			Targets: archivedOperationTargets(t, db, grant),
		}
		_, err = ApplyExternalModuleOperation(t.Context(), db, reservation)
		require.NoError(t, err)
		reservations, err := db.ListModuleOperationReservations(t.Context(), nil, grant.OrgID, moduleID, false)
		require.NoError(t, err)
		require.Len(t, reservations, 1)
		require.Equal(t, grant.PluginID, reservations[0].Namespace)
		require.Equal(t, grant.OperationID.String(), reservations[0].RelatedResourceID)

		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.SetModuleCatalogueStatus(t.Context(), tx, grant.OrgID, moduleID.String(), moduleversions.CatalogueArchived,
			grant.ActorID, "Retire reserved Module", module.ResourceVersion)
		require.ErrorContains(t, err, "exclusive operation reservations")
		require.NoError(t, tx.Rollback())

		current, err := db.GetModuleCatalogue(t.Context(), nil, grant.OrgID, moduleID.String(), GetModeDefault)
		require.NoError(t, err)
		require.Equal(t, moduleversions.CatalogueActive, current.Status)
	})

	t.Run("new operation prepare rejects an archived proposed target", func(t *testing.T) {
		db := externalOperationDatabase(t)
		prepare := externalGrantFixture(t, db)
		archivedModule := prepare.Intent.Modules[0]
		module, err := db.GetModuleCatalogue(t.Context(), nil, prepare.OrgID, archivedModule.String(), GetModeDefault)
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.SetModuleCatalogueStatus(t.Context(), tx, prepare.OrgID, archivedModule.String(), moduleversions.CatalogueArchived,
			prepare.ActorID, "Retire proposed target", module.ResourceVersion)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		proposed, err := ListProposedCoreModuleVersions(t.Context(), db, prepare.OrgID)
		require.NoError(t, err)
		for _, candidate := range proposed {
			require.NotEqual(t, archivedModule, candidate.ModuleUUID, "archived modules are omitted from native proposed-target inventory")
		}

		_, err = ApplyExternalGrantCommand(t.Context(), db, prepare)
		require.ErrorContains(t, err, "archived")
		_, err = GetExternalOperationGrant(t.Context(), db, prepare.OrgID, prepare.PluginID, prepare.OperationID)
		require.Error(t, err, "rejected preparation must not persist a grant")
	})
}

func TestPreparedOperationCannotReserveArchivedModule(t *testing.T) {
	db := externalOperationDatabase(t)
	prepare := externalGrantFixture(t, db)
	prepare.Intent.Actions = append(prepare.Intent.Actions, "module.reserve", "module.release")
	grant, err := ApplyExternalGrantCommand(t.Context(), db, prepare)
	require.NoError(t, err)

	moduleID := grant.Intent.Modules[0]
	module, err := db.GetModuleCatalogue(t.Context(), nil, grant.OrgID, moduleID.String(), GetModeDefault)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.SetModuleCatalogueStatus(t.Context(), tx, grant.OrgID, moduleID.String(), moduleversions.CatalogueArchived,
		grant.ActorID, "Retire prepared target", module.ResourceVersion)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	activate := prepare
	activate.Intent = nil
	activate.Action, activate.RequestID, activate.ExpectedRevision = "activate", "activate", grant.Revision
	grant, err = ApplyExternalGrantCommand(t.Context(), db, activate)
	require.NoError(t, err)
	reservation := ExternalModuleCommand{
		OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID,
		RequestID: "reserve-archived-module", Action: "reserve", GrantFence: grant.Revision, Reason: "Acquire prepared reservation",
		Targets: archivedOperationTargets(t, db, grant),
	}
	_, err = ApplyExternalModuleOperation(t.Context(), db, reservation)
	require.ErrorContains(t, err, "archived")
	reservations, err := db.ListModuleOperationReservations(t.Context(), nil, grant.OrgID, moduleID, false)
	require.NoError(t, err)
	require.Empty(t, reservations, "a target archived after prepare cannot acquire a new forward reservation")
}
