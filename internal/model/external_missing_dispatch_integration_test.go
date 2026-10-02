//go:build integration

package model

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMissingExternalDispatchWithdrawalFrozenScope(t *testing.T) {
	db := externalOperationDatabase(t)
	control, grant, _ := externalPinFixture(t, db)
	env := grant.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "never-recorded", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	// Current Pins block admission, not revocation of an unadmitted claim.
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, dispatch), "active Pin")
	control.Action, control.RequestID, control.ExpectedRevision = "suspend", "suspend", grant.Revision
	suspended, err := ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	require.ErrorContains(t, RecoverWithdrawnExternalDispatch(t.Context(), db, dispatch, grant.Revision), "fence changed")
	for _, mutate := range []struct {
		name string
		edit func(*ExternalDispatch)
	}{
		{"actor", func(d *ExternalDispatch) { d.ActorID = uuid.New() }},
		{"future-fence", func(d *ExternalDispatch) { d.Fence = suspended.Revision + 1 }},
		{"zero-fence", func(d *ExternalDispatch) { d.Fence = 0 }},
		{"action", func(d *ExternalDispatch) { d.Action = "external-deployment.rollback" }},
		{"environment", func(d *ExternalDispatch) { d.EnvironmentUUID = uuid.New() }},
		{"project", func(d *ExternalDispatch) { d.ProjectUUID = uuid.New() }},
		{"module", func(d *ExternalDispatch) { d.TargetVersions = map[string]uuid.UUID{uuid.NewString(): uuid.New()} }},
		{"target", func(d *ExternalDispatch) {
			d.TargetVersions = map[string]uuid.UUID{grant.Intent.Modules[0].String(): uuid.New()}
		}},
		{"org", func(d *ExternalDispatch) { d.OrgID = "other-org" }},
		{"plugin", func(d *ExternalDispatch) { d.PluginID = "other-plugin" }},
		{"operation", func(d *ExternalDispatch) { d.OperationID = uuid.New() }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			wrong := dispatch
			mutate.edit(&wrong)
			require.Error(t, RecoverWithdrawnExternalDispatch(t.Context(), db, wrong, suspended.Revision))
		})
	}
	require.NoError(t, RecoverWithdrawnExternalDispatch(t.Context(), db, dispatch, suspended.Revision))
	require.NoError(t, RecoverWithdrawnExternalDispatch(t.Context(), db, dispatch, suspended.Revision))
	wrong := dispatch
	wrong.RequestDigest = string(make([]byte, 64))
	require.Error(t, RecoverWithdrawnExternalDispatch(t.Context(), db, wrong, suspended.Revision))
	wrong = dispatch
	wrong.Approval = &ExternalDispatchApproval{ActorID: uuid.New(), Scope: "project"}
	require.ErrorContains(t, RecoverWithdrawnExternalDispatch(t.Context(), db, wrong, suspended.Revision), "different exact request")
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "resume", suspended.Revision
	resumed, err := ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	late := dispatch
	late.Fence = resumed.Revision
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, late), "different exact request")
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, ExternalDispatchCompletion{EnvironmentUUID: env.EnvironmentID, ProjectUUID: env.ProjectID, RequestDigest: dispatch.RequestDigest, NoEffect: true}))
	require.Error(t, RecordExternalDispatch(t.Context(), db, late), "settlement never removes the non-admitting identity")
	stored, err := GetExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID)
	require.NoError(t, err)
	require.Equal(t, dispatch, *stored)
}

func TestMissingExternalDispatchWithdrawSubmitRace(t *testing.T) {
	db := externalOperationDatabase(t)
	_, grant, begin := externalPinFixture(t, db)
	_, err := ApplyExternalPinCommand(t.Context(), db, begin, nil)
	require.NoError(t, err)
	env := grant.Intent.Environments[0]
	for i := range 12 {
		dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: uuid.NewString(), ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
		var admitted, withdrawn error
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Go(func() { <-start; admitted = RecordExternalDispatch(t.Context(), db, dispatch) })
		wait.Go(func() {
			<-start
			withdrawn = RecoverWithdrawnExternalDispatch(t.Context(), db, dispatch, grant.Revision)
		})
		close(start)
		wait.Wait()
		require.NoError(t, withdrawn, "iteration %d", i)
		if admitted != nil {
			require.ErrorContains(t, admitted, "withdrawn")
		}
		check := dispatch
		check.TargetVersions = nil
		require.ErrorContains(t, CheckExternalDispatch(t.Context(), db, check), "withdrawn", "late callback is blocked whichever request acquired the lock first")
		// A committed effect is authoritative, even if withdrawal won the Core
		// race. Trusted receipt evidence settles as that effect, not NoEffect.
		completion := ExternalDispatchCompletion{EnvironmentUUID: env.EnvironmentID, ProjectUUID: env.ProjectID, RequestDigest: dispatch.RequestDigest, DeploymentID: uuid.New(), Status: "failed"}
		require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, completion))
		require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, completion))
		absence := completion
		absence.DeploymentID, absence.Status, absence.NoEffect = uuid.Nil, "", true
		require.ErrorContains(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, absence), "immutable")
		require.Error(t, RecordExternalDispatch(t.Context(), db, dispatch), "late submit remains blocked after terminal settlement")
	}
}

func TestLegacyExternalDispatchWithdrawalRetainsOriginalIntent(t *testing.T) {
	db := externalOperationDatabase(t)
	control, grant, _ := externalPinFixture(t, db)
	env := grant.Intent.Environments[0]
	legacy := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "retained-before-pin-barriers", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint}
	// The pre-Pin-barrier writer persisted this JSON shape without Module
	// targets. Reproduce its INSERT, not a mutation of current immutable history.
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO external_operation_dispatches(org_id,plugin_id,operation_id,dispatch_id,fence,action,actor_id,request_digest,intent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, legacy.OrgID, legacy.PluginID, legacy.OperationID, legacy.DispatchID, legacy.Fence, legacy.Action, legacy.ActorID, legacy.RequestDigest, raw)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	control.Action, control.RequestID, control.ExpectedRevision, control.ActorID = "activate", "fresh-recovery-operator", grant.Revision, uuid.New()
	control.SafetyActivation = true
	current, err := ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	newMissing := legacy
	newMissing.DispatchID, newMissing.ActorID = "new-without-targets", current.ActorID
	require.ErrorContains(t, RecoverWithdrawnExternalDispatch(t.Context(), db, newMissing, current.Revision), "exact Module target", "legacy compatibility cannot manufacture an unbounded new tombstone")
	require.NoError(t, RecoverWithdrawnExternalDispatch(t.Context(), db, legacy, current.Revision))
	forged := legacy
	forged.ActorID = current.ActorID
	require.ErrorContains(t, RecoverWithdrawnExternalDispatch(t.Context(), db, forged, current.Revision), "different exact request")
	late := legacy
	late.ActorID, late.Fence, late.TargetVersions = current.ActorID, current.Revision, current.Intent.TargetVersions
	require.ErrorContains(t, RecordExternalDispatch(t.Context(), db, late), "different exact request")
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, legacy.OrgID, legacy.PluginID, legacy.OperationID, legacy.DispatchID, ExternalDispatchCompletion{EnvironmentUUID: env.EnvironmentID, ProjectUUID: env.ProjectID, RequestDigest: legacy.RequestDigest, NoEffect: true}))
	require.Error(t, RecordExternalDispatch(t.Context(), db, late), "late Submit remains blocked after conclusive settlement")
	stored, err := GetExternalDispatch(t.Context(), db, legacy.OrgID, legacy.PluginID, legacy.OperationID, legacy.DispatchID)
	require.NoError(t, err)
	require.Equal(t, legacy, *stored)
}
