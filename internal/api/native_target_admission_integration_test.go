//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func nativeTargetAdmissionFixture(t *testing.T) (model.Databaser, *model.Runner, model.DeploymentAdmission) {
	t.Helper()
	db, grant := pluginDeploymentBoundaryDatabase(t)
	body, err := json.Marshal(map[string]any{"id": "native_pool", "runner_configuration": nativeAgentTestConfiguration(t), "state_storage_configuration": map[string]any{"type": "s3", "bucket": "owned-states"}})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/runners", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	finalDeletionRouter(t, db).ServeHTTP(response, request)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	runner, err := db.GetRunner(t.Context(), nil, grant.OrgID, "native_pool", model.GetModeDefault)
	require.NoError(t, err)
	env, err := db.GetEnvironment(t.Context(), nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &model.EnvironmentPatch{RunnerId: opt.Of(runner.Id), UpdatedAt: time.Now()})
	require.NoError(t, err)
	state, err := json.Marshal(runner.StateStorageConfiguration)
	require.NoError(t, err)
	kind, digest, err := genclient.StateStorageConfigurationDigest(state)
	require.NoError(t, err)
	return db, runner, model.DeploymentAdmission{
		OrgID: env.OrgId, AdmissionID: uuid.New(), ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid, ActorID: grant.ActorID,
		RequestDigest: strings.Repeat("b", 64), Mode: "deploy", ModuleVersions: grant.Intent.TargetVersions, CarryForwardModuleVersions: grant.Intent.TargetVersions,
		NativeTarget: &model.NativeExecutionTargetReference{RunnerID: runner.Id, RunnerUUID: runner.Uuid, RuntimeVersion: "stellwerk.runner.rollout/v2", PipelineVersion: "stellwerk.pipeline/v2", ToolsDigest: strings.Repeat("a", 64), StateBackendType: string(kind), StateConfigurationDigest: digest},
	}
}

func nativeFirstAdmissionFixture(t *testing.T) (model.Databaser, *model.Runner, model.DeploymentAdmission) {
	t.Helper()
	db, runner, admission := nativeTargetAdmissionFixture(t)
	env := nativeFirstCreatedEnvironment(t, db, runner, "native-first")
	admission.EnvironmentUUID = env.Uuid
	return db, runner, admission
}

func nativeFirstCreatedEnvironment(t *testing.T, db model.Databaser, runner *model.Runner, id string) *model.Environment {
	t.Helper()
	nativeFirstRules(t, db, runner.OrgId, model.RunnerRule{RunnerId: runner.Id})
	router := nativeFirstRouter(t, db, &hmessaging.RecordingPublisher{})
	response := nativeFirstRequest(router, http.MethodPost, "/orgs/"+runner.OrgId+"/projects/project/envs", []byte(`{"id":"`+id+`","env_type_id":"development"}`))
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	env, err := db.GetEnvironment(t.Context(), nil, runner.OrgId, "project", id, model.GetModeDefault)
	require.NoError(t, err)
	require.True(t, env.NativeFirstEligible)
	return env
}

func TestNativeTargetAdmissionRequiresExactReference(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*model.DeploymentAdmission)
	}{
		{"missing", func(a *model.DeploymentAdmission) { a.NativeTarget = nil }},
		{"foreign UUID", func(a *model.DeploymentAdmission) { a.NativeTarget.RunnerUUID = uuid.New() }},
		{"foreign slug", func(a *model.DeploymentAdmission) { a.NativeTarget.RunnerID = "other" }},
		{"changed tools", func(a *model.DeploymentAdmission) { a.NativeTarget.ToolsDigest = strings.Repeat("c", 64) }},
		{"changed backend type", func(a *model.DeploymentAdmission) { a.NativeTarget.StateBackendType = "gcs" }},
		{"changed backend configuration", func(a *model.DeploymentAdmission) { a.NativeTarget.StateConfigurationDigest = strings.Repeat("c", 64) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			db, _, admission := nativeFirstAdmissionFixture(t)
			change.run(&admission)
			receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			_, conflict := model.IsErrConflict(err)
			require.True(t, conflict, "expected exact-target conflict: %v", err)
			require.Nil(t, receipt)
			require.Zero(t, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Zero(t, epoch)
		})
	}
}

func TestNativeTargetAdmissionAcceptsSupportedOperations(t *testing.T) {
	for _, mode := range []string{"deploy", "rollback", "destroy", "plan_only", "rollback_plan"} {
		t.Run(mode, func(t *testing.T) {
			db, _, admission := nativeFirstAdmissionFixture(t)
			admission.Mode = mode
			if mode == "destroy" {
				admission.ModuleVersions, admission.CarryForwardModuleVersions = map[string]uuid.UUID{}, nil
				_, err := db.UpdateEnvironment(t.Context(), nil, admission.OrgID, "project", "native-first", &model.EnvironmentPatch{Status: opt.Of(model.EnvironmentStatusDeleting), DeletionPolicy: &model.EnvironmentDeletionPolicy{}, UpdatedAt: time.Now()})
				require.NoError(t, err)
			}
			receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			require.NoError(t, err)
			require.Equal(t, int64(1), receipt.Epoch)
			replayed, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			require.NoError(t, err)
			require.Equal(t, receipt, replayed)
		})
	}
}

func TestNativeTargetAdmissionRejectsChangedProfile(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*model.Runner)
	}{
		{"disabled", func(r *model.Runner) { r.RunnerConfiguration["native_execution"] = nil }},
		{"old runtime", func(r *model.Runner) {
			r.RunnerConfiguration["native_execution"].(map[string]any)["runtime_version"] = "stellwerk.runner.rollout/v1"
		}},
		{"missing saved plan protocol", func(r *model.Runner) {
			r.RunnerConfiguration["supported_protocols"] = []string{"stellwerk.pipeline/v2"}
		}},
		{"missing pipeline protocol", func(r *model.Runner) {
			r.RunnerConfiguration["supported_protocols"] = []string{"stellwerk.pipeline.plan-apply/v2"}
		}},
		{"changed tools", func(r *model.Runner) {
			r.RunnerConfiguration["native_execution"].(map[string]any)["tools_digest"] = strings.Repeat("c", 64)
		}},
		{"inconsistent state type", func(r *model.Runner) { r.StateStorageType = "gcs" }},
		{"changed state configuration", func(r *model.Runner) { r.StateStorageConfiguration["bucket"] = "other-states" }},
		{"changed state type", func(r *model.Runner) { r.StateStorageType, r.StateStorageConfiguration["type"] = "gcs", "gcs" }},
		{"malformed stored state", func(r *model.Runner) { r.StateStorageConfiguration["bucket"] = nil }},
	} {
		t.Run(change.name, func(t *testing.T) {
			db, runner, admission := nativeFirstAdmissionFixture(t)
			change.run(runner)
			_, err := db.UpdateRunner(t.Context(), nil, admission.OrgID, runner.Id, &model.RunnerPatch{RunnerConfiguration: &runner.RunnerConfiguration, StateStorageType: opt.Of(runner.StateStorageType), StateStorageConfiguration: &runner.StateStorageConfiguration, UpdatedAt: time.Now()})
			require.NoError(t, err)
			receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
			_, conflict := model.IsErrConflict(err)
			require.True(t, conflict, "stored profile must refuse: %v", err)
			require.Nil(t, receipt)
			require.Zero(t, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Zero(t, epoch)
		})
	}
}

func TestNativeTargetAdmissionIgnoresCredentialAndCapacityUpdates(t *testing.T) {
	db, runner, admission := nativeFirstAdmissionFixture(t)
	runner.RunnerConfiguration["key"] = nativeAgentTestConfiguration(t)["key"]
	runner.RunnerConfiguration["max_parallel_jobs"] = 3
	_, err := db.UpdateRunner(t.Context(), nil, admission.OrgID, runner.Id, &model.RunnerPatch{RunnerConfiguration: &runner.RunnerConfiguration, Description: opt.Of("updated display description"), UpdatedAt: time.Now()})
	require.NoError(t, err)
	receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, int64(1), receipt.Epoch)
}

func TestNativeTargetAdmissionReplayPrecedesChangedRegistration(t *testing.T) {
	db, runner, admission := nativeFirstAdmissionFixture(t)
	receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	other := *runner
	other.Id = "other-native"
	created, err := db.CreateRunner(t.Context(), tx, &other)
	require.NoError(t, err)
	require.NotEqual(t, runner.Uuid, created.Uuid)
	require.NoError(t, tx.Commit())
	configuration := runner.RunnerConfiguration
	configuration["native_execution"] = nil
	_, err = db.UpdateRunner(t.Context(), nil, admission.OrgID, runner.Id, &model.RunnerPatch{RunnerConfiguration: &configuration, UpdatedAt: time.Now()})
	require.NoError(t, err)
	reassigned, err := db.UpdateEnvironment(t.Context(), nil, admission.OrgID, "project", "native-first", &model.EnvironmentPatch{RunnerId: opt.Of(created.Id), UpdatedAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, created.Id, reassigned.RunnerId.Must())
	replayed, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	changed := admission
	changedTarget := *admission.NativeTarget
	changed.NativeTarget = &changedTarget
	changed.NativeTarget.ToolsDigest = strings.Repeat("c", 64)
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, changed)
	require.ErrorContains(t, err, "different exact request")
	_, err = model.SettleDeploymentAdmission(t.Context(), db, admission.OrgID, admission.AdmissionID, admission.ProjectUUID, admission.EnvironmentUUID, admission.RequestDigest, "succeeded")
	require.NoError(t, err)
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.ErrorContains(t, err, "settled")
	admission.AdmissionID = uuid.New()
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	_, conflict := model.IsErrConflict(err)
	require.True(t, conflict, "fresh acceptance cannot reuse the disabled registration: %v", err)
}

func TestNativeTargetAdmissionSealedAbsenceRetainsExactIdentity(t *testing.T) {
	db, _, admission := nativeFirstAdmissionFixture(t)
	receipt, err := model.SealDeploymentAdmissionAbsence(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, "absent", receipt.Outcome)
	replayed, err := model.SealDeploymentAdmissionAbsence(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.ErrorContains(t, err, "settled")
	target := *admission.NativeTarget
	admission.NativeTarget = &target
	admission.NativeTarget.StateConfigurationDigest = strings.Repeat("c", 64)
	_, err = model.SealDeploymentAdmissionAbsence(t.Context(), db, admission)
	_, conflict := model.IsErrConflict(err)
	require.True(t, conflict)
	require.ErrorContains(t, err, "immutable")
}
