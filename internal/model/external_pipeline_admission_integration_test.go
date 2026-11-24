//go:build integration

package model

import (
	"encoding/json"
	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func externalPipelineAdmissionFixture(t *testing.T, aware bool) (Databaser, DeploymentAdmission, ExternalDispatch) {
	t.Helper()
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	var ref *p.PipelineReference
	if aware {
		forwardOnly := false
		reference := p.BuiltinReference()
		ref = &reference
		bound := command.Intent.Environments[0]
		command.Intent.ForwardOnly = &forwardOnly
		command.Intent.Pipelines = []ExternalPipelineBinding{{ProjectUUID: bound.ProjectID, EnvironmentUUID: bound.EnvironmentID, Forward: reference, Rollback: &reference}}
	}
	_, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	control := command
	control.Intent = nil
	control.SourceOperation = "controlWorkflow"
	control.Action = "activate"
	control.RequestID = "start"
	control.ExpectedRevision = 1
	grant, err := ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	bound := command.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: command.OrgID, PluginID: command.PluginID, OperationID: command.OperationID, ActorID: command.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "original", ProjectUUID: bound.ProjectID, EnvironmentUUID: bound.EnvironmentID, RequestDigest: strings.Repeat("b", 64), TargetVersions: grant.Intent.TargetVersions, Pipeline: ref}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	request := DeploymentAdmission{OrgID: dispatch.OrgID, AdmissionID: uuid.New(), ProjectUUID: bound.ProjectID, EnvironmentUUID: bound.EnvironmentID, ActorID: dispatch.ActorID, RequestDigest: strings.Repeat("c", 64), Mode: "deploy", ModuleVersions: dispatch.TargetVersions, ExternalDispatch: &ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}}
	return db, request, dispatch
}
func TestOrdinaryPipelineAdmissionExcludesOnlyExactOwnedDispatch(t *testing.T) {
	db, request, dispatch := externalPipelineAdmissionFixture(t, true)
	assertBarrier := func(candidate DeploymentAdmission) error {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() {
			_ = tx.Rollback()
		}()
		return checkOrdinaryExternalDispatchBarrier(t.Context(), db, tx, candidate, false)
	}
	require.NoError(t, assertBarrier(request))
	for _, field := range []string{"digest", "pipeline", "actor", "environment", "mode", "fence", "target"} {
		t.Run(field, func(t *testing.T) {
			raw, err := json.Marshal(request)
			require.NoError(t, err)
			var changed DeploymentAdmission
			require.NoError(t, json.Unmarshal(raw, &changed))
			switch field {
			case "digest":
				changed.ExternalDispatch.RequestDigest = strings.Repeat("d", 64)
			case "pipeline":
				changed.Pipeline = &PipelineAdmissionReference{PipelineID: uuid.New(), VersionID: uuid.New(), Version: 1, Digest: strings.Repeat("e", 64)}
			case "actor":
				changed.ActorID = uuid.New()
			case "environment":
				changed.EnvironmentUUID = uuid.New()
			case "mode":
				changed.Mode = "rollback"
			case "fence":
				changed.ExternalDispatch.Fence++
			case "target":
				for module := range changed.ModuleVersions {
					changed.ModuleVersions[module] = uuid.New()
					break
				}
			}
			require.ErrorContains(t, assertBarrier(changed), "exact external dispatch")
		})
	}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	other := dispatch
	other.DispatchID = "another"
	encoded, err := json.Marshal(other)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO external_operation_dispatches(org_id,plugin_id,operation_id,dispatch_id,fence,action,actor_id,request_digest,intent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, other.OrgID, other.PluginID, other.OperationID, other.DispatchID, other.Fence, other.Action, other.ActorID, other.RequestDigest, encoded)
	require.NoError(t, err)
	require.ErrorContains(t, checkOrdinaryExternalDispatchBarrier(t.Context(), db, tx, request, false), "another unresolved")
	require.NoError(t, tx.Rollback())
	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
	require.NoError(t, err)
	require.Positive(t, receipt.Epoch)
}
func TestFreshLegacyExternalDispatchCannotBypassProtectedPipelinePolicy(t *testing.T) {
	db, request, _ := externalPipelineAdmissionFixture(t, false)
	project, err := db.GetProjectByUuid(t.Context(), nil, request.OrgID, request.ProjectUUID, GetModeDefault)
	require.NoError(t, err)
	draft := pipelineTestDraft(t, db, project, request.ActorID)
	version := pipelineTestPublish(t, db, project, request.ActorID, draft, "protect")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, request.ActorID, p.PipelinePolicyWrite{Enabled: true, DefaultVersionId: &version.Id, AllowedVersionIds: []uuid.UUID{version.Id}})
	})
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
	require.ErrorContains(t, err, "requires a published pipeline")
}
func TestOrdinaryExternalAdmissionPreservesOwnedPendingPinAndPause(t *testing.T) {
	db := externalOperationDatabase(t)
	control, grant, pinCommand := externalPinFixture(t, db)
	_, err := ApplyExternalPinCommand(t.Context(), db, pinCommand, nil)
	require.NoError(t, err)
	bound := grant.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "owned-pending", ProjectUUID: bound.ProjectID, EnvironmentUUID: bound.EnvironmentID, RequestDigest: strings.Repeat("b", 64), TargetVersions: grant.Intent.TargetVersions}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	request := DeploymentAdmission{OrgID: dispatch.OrgID, AdmissionID: uuid.New(), ProjectUUID: bound.ProjectID, EnvironmentUUID: bound.EnvironmentID, ActorID: dispatch.ActorID, RequestDigest: strings.Repeat("c", 64), Mode: "deploy", ModuleVersions: dispatch.TargetVersions, ExternalDispatch: &ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}}
	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err, "the operation's exact pending override remains valid under ordinary admission")
	control.Action = "suspend"
	control.RequestID = "pause"
	control.ExpectedRevision = grant.Revision
	_, err = ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	replay, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err, "pausing business orchestration does not revoke an admitted pipeline's approval or repair boundary")
	require.Equal(t, receipt, replay)
	fresh := request
	fresh.AdmissionID = uuid.New()
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, fresh)
	require.ErrorContains(t, err, "actor and fence")
}
func TestFreshOrdinaryExternalAdmissionRejectsAdvancedGrantFence(t *testing.T) {
	db, request, dispatch := externalPipelineAdmissionFixture(t, true)
	grant, err := GetExternalOperationGrant(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID)
	require.NoError(t, err)
	command := ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, SourceOperation: "controlWorkflow", SourceDigest: strings.Repeat("e", 64), RequestID: "pause-before-guard", ExpectedRevision: grant.Revision, Action: "suspend", Reason: "Pause before admission"}
	paused, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	command.RequestID = "resume-before-guard"
	command.ExpectedRevision = paused.Revision
	command.Action = "activate"
	_, err = ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request, DeploymentAdmissionPolicy{AllowFreshProposed: true})
	require.ErrorContains(t, err, "actor and fence", "an active newer grant cannot authorize first admission with an old recorded generation")
}
