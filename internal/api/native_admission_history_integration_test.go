//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func settleNativeAdmission(t *testing.T, db model.Databaser, value model.DeploymentAdmission, outcome string) *model.NativeAdmissionWatermark {
	t.Helper()
	receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, value)
	require.NoError(t, err)
	replayed, err := model.AuthorizeDeploymentAdmission(t.Context(), db, value)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = model.SettleDeploymentAdmission(t.Context(), db, value.OrgID, value.AdmissionID, value.ProjectUUID, value.EnvironmentUUID, value.RequestDigest, outcome)
	require.NoError(t, err)
	return &model.NativeAdmissionWatermark{AdmissionID: value.AdmissionID, Epoch: receipt.Epoch}
}

func TestNativeAdmissionWatermarkTracksPlanAndTerminalKnowledge(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	first := settleNativeAdmission(t, db, admission, "succeeded")
	latest := first
	for _, step := range []struct{ mode, outcome string }{{"plan_only", "succeeded"}, {"deploy", "failed"}, {"rollback_plan", "succeeded"}} {
		next := admission
		next.AdmissionID, next.Mode, next.NativeAdmissionWatermark = uuid.New(), step.mode, latest
		latest = settleNativeAdmission(t, db, next, step.outcome)
		absent := next
		absent.AdmissionID, absent.NativeAdmissionWatermark = uuid.New(), latest
		_, err := model.SealDeploymentAdmissionAbsence(t.Context(), db, absent)
		require.NoError(t, err)
	}
	before := admissionGuards(t, db, admission.OrgID)
	epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
	require.NoError(t, err)
	for _, stale := range []*model.NativeAdmissionWatermark{nil, first, {AdmissionID: uuid.New(), Epoch: latest.Epoch}, {AdmissionID: latest.AdmissionID, Epoch: latest.Epoch + 1}} {
		candidate := admission
		candidate.AdmissionID, candidate.NativeAdmissionWatermark = uuid.New(), stale
		_, err := model.AuthorizeDeploymentAdmission(t.Context(), db, candidate)
		conflict, ok := model.IsErrConflict(err)
		require.True(t, ok, "%v", err)
		require.Equal(t, "native_admission_history_advanced", conflict.Code)
		require.Equal(t, before, admissionGuards(t, db, admission.OrgID))
		unchanged, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
		require.NoError(t, err)
		require.Equal(t, epoch, unchanged, "direct Core refusal does not insert an absent tombstone")
	}
	admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), latest
	final := settleNativeAdmission(t, db, admission, "succeeded")
	require.Equal(t, latest.Epoch+2, final.Epoch, "checked absence gaps are not predecessor knowledge")
}

func TestNativeAdmissionDoesNotSkipLatestUnsettledGuard(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	first := settleNativeAdmission(t, db, admission, "succeeded")
	admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), first
	active, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	candidate := admission
	candidate.AdmissionID = uuid.New()
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, candidate)
	conflict, ok := model.IsErrConflict(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, model.EnvironmentBusy, conflict.Code)
	replayed, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, active, replayed)
	candidate.AdmissionID = admission.AdmissionID
	candidate.NativeAdmissionWatermark = &model.NativeAdmissionWatermark{AdmissionID: uuid.New(), Epoch: first.Epoch}
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, candidate)
	require.ErrorContains(t, err, "different exact request")
	require.Equal(t, 2, admissionGuards(t, db, admission.OrgID))
}

func TestNativeAdmissionRefusesMalformedLatestRetainedEvidence(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	sqlDB := db.(model.Tx)
	admission.NativeAdmissionWatermark = settleNativeAdmission(t, db, admission, "succeeded")
	admission.AdmissionID = uuid.New()
	latest := settleNativeAdmission(t, db, admission, "succeeded")
	original, err := json.Marshal(admission)
	require.NoError(t, err)
	for _, change := range []struct{ name, assignment string }{
		{"legacy latest", `intent=intent-'native_target'`},
		{"foreign organization", `intent=jsonb_set(intent,'{org_id}','"other"')`},
		{"foreign project", `intent=jsonb_set(intent,'{project_uuid}',to_jsonb($3::text))`},
		{"foreign environment", `intent=jsonb_set(intent,'{environment_uuid}',to_jsonb($3::text))`},
		{"wrong admission identity", `intent=jsonb_set(intent,'{admission_id}',to_jsonb($3::text))`},
		{"malformed native profile", `intent=jsonb_set(intent,'{native_target,tools_digest}','"bad"')`},
		{"different native custody", `intent=jsonb_set(intent,'{native_target,runner_uuid}',to_jsonb($3::text))`},
		{"unchecked settlement", `settled_at=NULL`},
	} {
		t.Run(change.name, func(t *testing.T) {
			args := []any{admission.OrgID, admission.AdmissionID}
			if strings.Contains(change.assignment, "$3") {
				args = append(args, uuid.New().String())
			}
			_, err := sqlDB.ExecContext(t.Context(), `UPDATE deployment_admission_guards SET `+change.assignment+` WHERE org_id=$1 AND admission_id=$2`, args...)
			require.NoError(t, err)
			defer func() {
				_, err := sqlDB.ExecContext(t.Context(), `UPDATE deployment_admission_guards SET intent=$3,settled_at=now() WHERE org_id=$1 AND admission_id=$2`, admission.OrgID, admission.AdmissionID, original)
				require.NoError(t, err)
			}()
			candidate := admission
			candidate.AdmissionID, candidate.NativeAdmissionWatermark = uuid.New(), latest
			_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, candidate)
			conflict, ok := model.IsErrConflict(err)
			require.True(t, ok, "%v", err)
			require.Equal(t, "native_admission_history_advanced", conflict.Code)
			require.Equal(t, 2, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, latest.Epoch, epoch)
		})
	}
	t.Run("ambiguous latest epoch", func(t *testing.T) {
		_, err := sqlDB.ExecContext(t.Context(), `INSERT INTO deployment_admission_guards(org_id,admission_id,environment_uuid,project_uuid,intent,epoch,outcome,settled_at)
 SELECT org_id,$3::uuid,environment_uuid,project_uuid,jsonb_set(intent,'{admission_id}',to_jsonb($3::uuid::text)),epoch,outcome,settled_at
 FROM deployment_admission_guards WHERE org_id=$1 AND admission_id=$2`, admission.OrgID, admission.AdmissionID, uuid.New())
		require.NoError(t, err)
		candidate := admission
		candidate.AdmissionID, candidate.NativeAdmissionWatermark = uuid.New(), latest
		_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, candidate)
		conflict, ok := model.IsErrConflict(err)
		require.True(t, ok, "%v", err)
		require.Equal(t, "native_admission_history_advanced", conflict.Code)
		require.Equal(t, 3, admissionGuards(t, db, admission.OrgID))
		epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
		require.NoError(t, err)
		require.Equal(t, latest.Epoch, epoch)
	})
}

func TestNativeAdmissionRefusesBothUnqualifiedAndLegacyRealmTransfers(t *testing.T) {
	db, runner, admission := nativeTargetAdmissionFixture(t)
	_, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.ErrorContains(t, err, "eligible Native-first")
	eligible := nativeFirstCreatedEnvironment(t, db, runner, "native-first")
	admission.EnvironmentUUID, admission.NativeTarget = eligible.Uuid, nil
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.ErrorContains(t, err, "eligible Native-first")
	legacy := nativeFirstExtraRunner(t, db, admission.OrgID, "legacy_pool", false)
	_, err = db.UpdateEnvironment(t.Context(), nil, admission.OrgID, "project", eligible.Id, &model.EnvironmentPatch{RunnerId: opt.Of(legacy.Id), UpdatedAt: time.Now()})
	require.NoError(t, err)
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.ErrorContains(t, err, "eligible Native-first")
	require.Zero(t, admissionGuards(t, db, admission.OrgID))
}

func TestNativeFirstAdmissionSerializesTwoBootstrapRequests(t *testing.T) {
	db, _, first := nativeFirstAdmissionFixture(t)
	second := first
	second.AdmissionID = uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
	require.NoError(t, model.CheckEnvironmentAdmissionEpoch(ctx, db, held, first.OrgID, first.EnvironmentUUID, 0))
	results := make(chan nativeAdmissionOutcome, 2)
	for _, value := range []model.DeploymentAdmission{first, second} {
		go func() {
			receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, value)
			results <- nativeAdmissionOutcome{receipt, err}
		}()
	}
	pid := waitFinalDeletionLock(t, db, holder, 0)
	waitFinalDeletionLock(t, db, holder, pid)
	require.NoError(t, held.Commit())
	one, two := <-results, <-results
	if one.err != nil {
		one, two = two, one
	}
	require.NoError(t, one.err)
	require.Equal(t, int64(1), one.receipt.Epoch)
	conflict, ok := model.IsErrConflict(two.err)
	require.True(t, ok, "%v", two.err)
	require.Equal(t, model.EnvironmentBusy, conflict.Code)
	require.Nil(t, two.receipt)
	require.Equal(t, 1, admissionGuards(t, db, first.OrgID))
}

func nativeHistoryGrant(t *testing.T, db model.Databaser, admission model.DeploymentAdmission, reference *p.PipelineReference) (model.ExternalGrantCommand, *model.ExternalOperationGrant) {
	t.Helper()
	var modules []uuid.UUID
	for id := range admission.ModuleVersions {
		modules = append(modules, uuid.MustParse(id))
	}
	command := model.ExternalGrantCommand{OrgID: admission.OrgID, PluginID: "dev.example.history", OperationID: uuid.New(), ActorID: admission.ActorID,
		SourceOperation: "prepareWorkflow", SourceDigest: strings.Repeat("a", 64), RequestID: "prepare", Action: "prepare", Reason: "Native history qualification",
		Intent: &model.ExternalOperationIntent{Fingerprint: strings.Repeat("b", 64), Modules: modules, TargetVersions: admission.ModuleVersions,
			Environments: []model.ExternalOperationEnvironment{{ProjectID: admission.ProjectUUID, EnvironmentID: admission.EnvironmentUUID}}, Actions: []string{"external-deployment.admit", "external-deployment.rollback"}}}
	if reference != nil {
		forwardOnly := false
		command.Intent.ForwardOnly = &forwardOnly
		command.Intent.Pipelines = []model.ExternalPipelineBinding{{ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, Forward: *reference, Rollback: reference}}
	}
	grant, err := model.ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	command.Intent, command.Action, command.RequestID, command.ExpectedRevision = nil, "activate", "start", grant.Revision
	grant, err = model.ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	return command, grant
}

func TestNativeExternalHistoryRetainsReleasedGrantAndRefusesUnrepresentedEffect(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	command, grant := nativeHistoryGrant(t, db, admission, nil)
	dispatch := model.ExternalDispatch{OrgID: admission.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: admission.ActorID, Fence: grant.Revision,
		Action: "external-deployment.admit", DispatchID: "represented", ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID,
		RequestDigest: strings.Repeat("d", 64), TargetVersions: admission.ModuleVersions}
	require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
	admission.ExternalDispatch = &model.ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}
	latest := settleNativeAdmission(t, db, admission, "succeeded")
	completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}
	require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, completion))
	command.Action, command.RequestID, command.ExpectedRevision = "release", "release", grant.Revision
	grant, err := model.ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	admission.AdmissionID, admission.ExternalDispatch, admission.NativeAdmissionWatermark = uuid.New(), nil, latest
	latest = settleNativeAdmission(t, db, admission, "succeeded")
	command.Action, command.RequestID, command.ExpectedRevision = "activate", "resume", grant.Revision
	grant, err = model.ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	dispatch.DispatchID, dispatch.Fence, dispatch.RequestDigest = "absent", grant.Revision, strings.Repeat("f", 64)
	require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
	require.NoError(t, model.WithdrawExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID))
	absent := model.ExternalDispatchCompletion{ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, RequestDigest: dispatch.RequestDigest, NoEffect: true}
	require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, absent))
	admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), latest
	latest = settleNativeAdmission(t, db, admission, "succeeded")
	dispatch.DispatchID, dispatch.Fence, dispatch.RequestDigest = "unrepresented", grant.Revision, strings.Repeat("e", 64)
	require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
	completion.DeploymentID, completion.RequestDigest = uuid.New(), dispatch.RequestDigest
	require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, completion))
	admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), latest
	before := admissionGuards(t, db, admission.OrgID)
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	conflict, ok := model.IsErrConflict(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, "native_admission_history_advanced", conflict.Code)
	require.Equal(t, before, admissionGuards(t, db, admission.OrgID))
}

func TestNativeExternalHistoryRefusesMalformedCompletionAndScope(t *testing.T) {
	for _, change := range []struct {
		name, assignment string
		allowed          bool
	}{
		{"valid builtin after grant release", `intent=intent`, true},
		{"missing completion", `completion=NULL`, false},
		{"foreign completion project", `completion=jsonb_set(completion,'{ProjectUUID}',to_jsonb($5::text))`, false},
		{"wrong completion digest", `completion=jsonb_set(completion,'{RequestDigest}','"bad"')`, false},
		{"missing absence discriminator", `completion=completion-'NoEffect'`, false},
		{"absence with deployment", `completion=jsonb_set(completion,'{NoEffect}','true')`, false},
		{"missing module targets", `intent=intent-'target_versions'`, false},
		{"invalid builtin", `intent=jsonb_set(intent,'{pipeline,builtin}','"unknown"')`, false},
		{"changed builtin digest", `intent=jsonb_set(intent,'{pipeline,digest}','"bad"')`, false},
		{"builtin and published identity", `intent=jsonb_set(intent,'{pipeline,version_id}',to_jsonb($5::text))`, false},
	} {
		t.Run(change.name, func(t *testing.T) {
			db, _, admission := nativeFirstAdmissionFixture(t)
			sqlDB := db.(model.Tx)
			reference := p.BuiltinReference()
			command, grant := nativeHistoryGrant(t, db, admission, &reference)
			dispatch := model.ExternalDispatch{OrgID: admission.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: admission.ActorID, Fence: grant.Revision,
				Action: "external-deployment.admit", DispatchID: "represented", ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID,
				RequestDigest: strings.Repeat("d", 64), TargetVersions: admission.ModuleVersions, Pipeline: &reference}
			require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
			admission.ExternalDispatch = &model.ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}
			latest := settleNativeAdmission(t, db, admission, "succeeded")
			completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}
			require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, completion))
			command.Action, command.RequestID, command.ExpectedRevision = "release", "release", grant.Revision
			_, err := model.ApplyExternalGrantCommand(t.Context(), db, command)
			require.NoError(t, err)
			args := []any{dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID}
			if strings.Contains(change.assignment, "$5") {
				args = append(args, uuid.New().String())
			}
			_, err = sqlDB.ExecContext(t.Context(), `UPDATE external_operation_dispatches SET `+change.assignment+` WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, args...)
			require.NoError(t, err)
			admission.AdmissionID, admission.ExternalDispatch, admission.NativeAdmissionWatermark = uuid.New(), nil, latest
			receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			if change.allowed {
				require.NoError(t, err)
				require.Equal(t, latest.Epoch+1, receipt.Epoch)
				return
			}
			conflict, ok := model.IsErrConflict(err)
			require.True(t, ok, "%v", err)
			require.Equal(t, "native_admission_history_advanced", conflict.Code)
			require.Nil(t, receipt)
			require.Equal(t, 1, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, latest.Epoch, epoch)
		})
	}
}

func TestNativeExternalHistoryMatchesPublishedRecipeAfterGrantRelease(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	project, err := model.LockPipelineProject(t.Context(), db, tx, admission.OrgID, "project")
	require.NoError(t, err)
	draft, err := model.CreatePipelineDraft(t.Context(), tx, project, admission.ActorID, p.PipelineDraftCreate{Slug: "history", Name: "Published history", Definition: p.PipelineRecipe{Strategy: p.Apply, Steps: []p.PipelineRecipeStep{}}})
	require.NoError(t, err)
	version, err := model.PublishPipeline(t.Context(), tx, project, draft.Id, admission.ActorID, p.PipelinePublish{ExpectedResourceVersion: draft.ResourceVersion, RequestKey: "publish"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	reference := p.PipelineReference{VersionId: &version.Id, Digest: version.Digest}
	command, grant := nativeHistoryGrant(t, db, admission, &reference)
	dispatch := model.ExternalDispatch{OrgID: admission.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: admission.ActorID, Fence: grant.Revision,
		Action: "external-deployment.admit", DispatchID: "published", ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID,
		RequestDigest: strings.Repeat("d", 64), TargetVersions: admission.ModuleVersions, Pipeline: &reference}
	require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
	admission.Pipeline = &model.PipelineAdmissionReference{PipelineID: version.PipelineId, VersionID: version.Id, Version: version.Version, Digest: version.Digest}
	admission.ExternalDispatch = &model.ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}
	latest := settleNativeAdmission(t, db, admission, "succeeded")
	completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}
	require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, completion))
	command.Action, command.RequestID, command.ExpectedRevision = "release", "release", grant.Revision
	_, err = model.ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	admission.AdmissionID, admission.ExternalDispatch, admission.NativeAdmissionWatermark = uuid.New(), nil, latest
	latest = settleNativeAdmission(t, db, admission, "succeeded")
	for _, field := range []string{"version_id", "digest", "builtin"} {
		t.Run(field, func(t *testing.T) {
			changed := reference
			switch field {
			case "version_id":
				other := uuid.New()
				changed.VersionId = &other
			case "digest":
				changed.Digest = strings.Repeat("e", 64)
			case "builtin":
				builtin := p.Default
				changed.Builtin = &builtin
			}
			encoded, err := json.Marshal(changed)
			require.NoError(t, err)
			sqlDB := db.(model.Tx)
			_, err = sqlDB.ExecContext(t.Context(), `UPDATE external_operation_dispatches SET intent=jsonb_set(intent,'{pipeline}',$5::jsonb) WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, encoded)
			require.NoError(t, err)
			defer func() {
				original, err := json.Marshal(reference)
				require.NoError(t, err)
				_, err = sqlDB.ExecContext(t.Context(), `UPDATE external_operation_dispatches SET intent=jsonb_set(intent,'{pipeline}',$5::jsonb) WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, original)
				require.NoError(t, err)
			}()
			admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), latest
			_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			conflict, ok := model.IsErrConflict(err)
			require.True(t, ok, "%v", err)
			require.Equal(t, "native_admission_history_advanced", conflict.Code)
			require.Equal(t, 2, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, latest.Epoch, epoch)
		})
	}
}

func TestNativeExternalHistoryChecksOlderRepresentingGuard(t *testing.T) {
	for _, change := range []struct {
		name, assignment string
		allowed          bool
	}{
		{"empty target", `intent=jsonb_set(intent,'{native_target}','{}')`, false},
		{"wrong admission identity", `intent=jsonb_set(intent,'{admission_id}',to_jsonb($3::text))`, false},
		{"malformed ordinary digest", `intent=jsonb_set(intent,'{request_digest}','"bad"')`, false},
		{"numeric ordinary digest", `intent=jsonb_set(intent,'{request_digest}',to_jsonb(repeat('1',64)::numeric))`, false},
		{"foreign runner custody", `intent=jsonb_set(intent,'{native_target,runner_uuid}',to_jsonb($3::text))`, false},
		{"changed backend custody", `intent=jsonb_set(intent,'{native_target,state_backend_type}','"gcs"')`, false},
		{"changed configuration custody", `intent=jsonb_set(intent,'{native_target,state_configuration_digest}',to_jsonb(repeat('f',64)))`, false},
		{"invalid runner slug", `intent=jsonb_set(intent,'{native_target,runner_id}','"invalid slug"')`, false},
		{"boolean runner slug", `intent=jsonb_set(intent,'{native_target,runner_id}','true')`, false},
		{"invalid runtime", `intent=jsonb_set(intent,'{native_target,runtime_version}','"unsupported"')`, false},
		{"invalid pipeline", `intent=jsonb_set(intent,'{native_target,pipeline_version}','"unsupported"')`, false},
		{"malformed tools digest", `intent=jsonb_set(intent,'{native_target,tools_digest}','"bad"')`, false},
		{"numeric tools digest", `intent=jsonb_set(intent,'{native_target,tools_digest}',to_jsonb(repeat('1',64)::numeric))`, false},
		{"historical tools differ", `intent=jsonb_set(intent,'{native_target,tools_digest}',to_jsonb(repeat('f',64)))`, true},
	} {
		t.Run(change.name, func(t *testing.T) {
			db, _, admission := nativeFirstAdmissionFixture(t)
			first := settleNativeAdmission(t, db, admission, "succeeded")
			admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), first
			command, grant := nativeHistoryGrant(t, db, admission, nil)
			dispatch := model.ExternalDispatch{OrgID: admission.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: admission.ActorID, Fence: grant.Revision,
				Action: "external-deployment.admit", DispatchID: "older-represented", ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID,
				RequestDigest: strings.Repeat("d", 64), TargetVersions: admission.ModuleVersions}
			require.NotEqual(t, admission.RequestDigest, dispatch.RequestDigest, "ordinary and external digests have different identities")
			require.NoError(t, model.RecordExternalDispatch(t.Context(), db, dispatch))
			admission.ExternalDispatch = &model.ExternalAdmissionDispatch{PluginID: dispatch.PluginID, OperationID: dispatch.OperationID, DispatchID: dispatch.DispatchID, Fence: dispatch.Fence, RequestDigest: dispatch.RequestDigest}
			represented := settleNativeAdmission(t, db, admission, "succeeded")
			completion := model.ExternalDispatchCompletion{DeploymentID: uuid.New(), ProjectUUID: admission.ProjectUUID, EnvironmentUUID: admission.EnvironmentUUID, RequestDigest: dispatch.RequestDigest, Status: "succeeded"}
			require.NoError(t, model.CompleteExternalDispatch(t.Context(), db, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, completion))
			command.Action, command.RequestID, command.ExpectedRevision = "release", "release", grant.Revision
			_, err := model.ApplyExternalGrantCommand(t.Context(), db, command)
			require.NoError(t, err)
			admission.AdmissionID, admission.ExternalDispatch, admission.NativeAdmissionWatermark = uuid.New(), nil, represented
			latest := settleNativeAdmission(t, db, admission, "succeeded")
			args := []any{admission.OrgID, represented.AdmissionID}
			if strings.Contains(change.assignment, "$3") {
				args = append(args, uuid.New().String())
			}
			_, err = db.(model.Tx).ExecContext(t.Context(), `UPDATE deployment_admission_guards SET `+change.assignment+` WHERE org_id=$1 AND admission_id=$2`, args...)
			require.NoError(t, err)
			admission.AdmissionID, admission.NativeAdmissionWatermark = uuid.New(), latest
			receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			guards := admissionGuards(t, db, admission.OrgID)
			epoch, epochErr := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, epochErr)
			t.Logf("older reference outcome: receipt=%+v error=%v guards_before=3 guards_after=%d epoch_before=%d epoch_after=%d", receipt, err, guards, latest.Epoch, epoch)
			if change.allowed {
				require.NoError(t, err)
				require.Equal(t, latest.Epoch+1, receipt.Epoch)
				require.Equal(t, 4, guards)
				return
			}
			conflict, ok := model.IsErrConflict(err)
			require.True(t, ok, "%v", err)
			require.Equal(t, "native_admission_history_advanced", conflict.Code)
			require.Nil(t, receipt)
			require.Equal(t, 3, guards)
			require.Equal(t, latest.Epoch, epoch)
		})
	}
}
