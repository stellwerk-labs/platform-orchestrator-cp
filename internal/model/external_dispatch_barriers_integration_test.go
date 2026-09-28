//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stretchr/testify/require"
)

func TestExternalDispatchPinHistoryAndConclusiveRecovery(t *testing.T) {
	db := externalOperationDatabase(t)
	_, grant, begin := externalPinFixture(t, db)
	env := grant.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "dispatch-1", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	// Active protection is not an approval decision and cannot be skipped even
	// when the caller requires no approval.
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, dispatch), "active Pin")
	boundaries := map[string]uuid.UUID{}
	for _, target := range begin.Targets {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		pin, err := db.GetEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, target.PinID, GetModeDefault)
		require.NoError(t, err)
		boundaries[pin.ModuleUUID.String()] = pin.ActivationEventID
		_, err = db.TransitionEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, pin.ID, pin.ResourceVersion, moduleversions.PinRemoved, "unpin", uuid.New(), "user", "Another scoped operator removes protection", nil, nil, nil)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	dispatch.ApprovedPins = map[string]uuid.UUID{}
	for module := range grant.Intent.TargetVersions {
		dispatch.ApprovedPins[module] = uuid.Nil
	}
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, dispatch), "latest Pin activation", "removal must not revive pre-Pin approval")
	dispatch.ApprovedPins = boundaries
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	check := dispatch
	check.TargetVersions, check.ApprovedPins = nil, nil
	require.NoError(t, CheckExternalDispatch(t.Context(), db, check))
	createPin := func() error {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		module := grant.Intent.Modules[0]
		_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, env.ProjectID, "project", env.EnvironmentID, "development", module, *grant.BaselineDefaults[module.String()], grant.ActorID, "user", "Protect stable version", nil, false)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	require.ErrorContains(t, createPin(), "unresolved external Deployment")
	absent := ExternalDispatchCompletion{EnvironmentUUID: env.EnvironmentID, ProjectUUID: env.ProjectID, RequestDigest: dispatch.RequestDigest, NoEffect: true}
	require.ErrorContains(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, absent), "withdrawn", "a momentarily absent DP receipt does not prove no work is in flight")
	require.NoError(t, WithdrawExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID))
	require.ErrorContains(t, CheckExternalDispatch(t.Context(), db, check), "withdrawn")
	require.ErrorContains(t, createPin(), "unresolved", "withdrawal is not completion evidence")
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, absent))
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, absent))
	changed := absent
	changed.NoEffect, changed.Status, changed.DeploymentID = false, "succeeded", uuid.New()
	require.ErrorContains(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, changed), "immutable")
	require.NoError(t, createPin(), "only conclusive completion frees Pin creation")
	require.ErrorContains(t, CheckExternalDispatch(t.Context(), db, check), "settled")
}

func TestExternalDispatchPinNotesAndNoApprovalPolicy(t *testing.T) {
	db := externalOperationDatabase(t)
	_, grant, begin := externalPinFixture(t, db)
	pins, err := ApplyExternalPinCommand(t.Context(), db, begin, nil)
	require.NoError(t, err)
	for _, pin := range pins {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = db.AppendModuleVersionPinNote(t.Context(), tx, grant.OrgID, pin.ID, uuid.New(), "user", "Additional context, not consent")
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	env := grant.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "no-approval-policy", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch), "explicitly owned overrides do not invent approval requirements for approval:none")
	check := dispatch
	check.TargetVersions = nil
	require.NoError(t, CheckExternalDispatch(t.Context(), db, check))
}
