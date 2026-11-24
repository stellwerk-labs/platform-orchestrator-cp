//go:build integration

package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func TestRollbackDefaultResolutionRequiresExplicitOptIn(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	environment := grant.Intent.Environments[0]
	now := time.Now().UTC()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.CreateRunner(t.Context(), tx, &model.Runner{OrgId: grant.OrgID, Id: "pipeline-runner", RunnerType: "kubernetes-agent",
		RunnerConfiguration: map[string]any{"supported_protocols": []string{"stellwerk.pipeline/v1", "pipeline.recipe.v1"}},
		StateStorageType:    "kubernetes", StateStorageConfiguration: map[string]any{}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE envs SET runner_id=$1 WHERE uuid=$2", "pipeline-runner", environment.EnvironmentID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	server := &Server{Database: db}
	selected := pluginDeploymentEnvironment{ProjectID: "project", EnvironmentID: "development", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID}
	_, err = server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, false, false)
	require.ErrorContains(t, err, "rollback_pipeline_required")
	bindings, err := server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, false, true)
	require.NoError(t, err)
	require.Equal(t, p.BuiltinReference().Digest, bindings.Forward.Digest)
	require.Equal(t, p.BuiltinReference().Digest, bindings.Rollback.Digest)
	require.False(t, bindings.ForwardOnly)
	bindings, err = server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, true, false)
	require.NoError(t, err)
	require.True(t, bindings.ForwardOnly)
	require.Nil(t, bindings.Rollback)
}

func TestNativePipelineChoicesResolvePublishedDirectionalDefaults(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	environment := grant.Intent.Environments[0]
	config := nativeAgentTestConfiguration(t)
	config["capabilities"] = []string{"local.exec", "pipeline.recipe.v1"}
	now := time.Now().UTC()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.CreateRunner(t.Context(), tx, &model.Runner{OrgId: grant.OrgID, Id: "native-pipelines", RunnerType: string(RunnerTypeNativeAgent),
		RunnerConfiguration: config, StateStorageType: "kubernetes", StateStorageConfiguration: map[string]any{}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE envs SET runner_id=$1 WHERE uuid=$2", "native-pipelines", environment.EnvironmentID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	server := &Server{Database: db}
	selected := pluginDeploymentEnvironment{ProjectID: "project", EnvironmentID: "development", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID}
	builtin, err := server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, false, true)
	require.NoError(t, err)
	require.Equal(t, pipelineJSON[PipelineReference](p.BuiltinReference()), builtin.Forward)
	require.Equal(t, &builtin.Forward, builtin.Rollback)

	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	project, err := model.LockPipelineProject(t.Context(), db, tx, grant.OrgID, selected.ProjectID)
	require.NoError(t, err)
	versions := []p.PublishedPipelineVersion{}
	for _, slug := range []string{"forward", "rollback", "checked"} {
		recipe := p.PipelineRecipe{Strategy: p.Reviewed, Steps: []p.PipelineRecipeStep{}}
		if slug == "checked" {
			recipe.Steps = []p.PipelineRecipeStep{{Id: "policy", ToolId: p.ResourcePolicy, Phase: p.PlanCheck, Parameters: map[string]any{}}}
		}
		draft, err := model.CreatePipelineDraft(t.Context(), tx, project, grant.ActorID, p.PipelineDraftCreate{Slug: slug, Name: "Native reviewed", Definition: recipe})
		require.NoError(t, err)
		version, err := model.PublishPipeline(t.Context(), tx, project, draft.Id, grant.ActorID,
			p.PipelinePublish{ExpectedResourceVersion: draft.ResourceVersion, RequestKey: slug})
		require.NoError(t, err)
		versions = append(versions, version)
	}
	_, err = model.SetPipelinePolicy(t.Context(), tx, project, grant.ActorID, p.PipelinePolicyWrite{
		Enabled: true, AllowedVersionIds: []uuid.UUID{versions[0].Id, versions[1].Id, versions[2].Id}, DefaultVersionId: &versions[1].Id,
		EnvironmentDefaultVersionIds: map[string]uuid.UUID{environment.EnvironmentID.String(): versions[0].Id},
		RollbackDefaults:             &p.PipelineRollbackDefaults{DefaultVersionId: &versions[1].Id},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	choices, err := server.readPluginPipelineChoices(t.Context(), grant.OrgID, selected)
	require.NoError(t, err)
	require.Len(t, choices.Versions, 3)
	bindings, err := server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, false, true)
	require.NoError(t, err)
	require.NotEqual(t, versions[0].Id, versions[1].Id)
	require.Equal(t, PipelineReference{VersionId: &versions[0].Id, Digest: versions[0].Digest}, bindings.Forward)
	require.Equal(t, &PipelineReference{VersionId: &versions[1].Id, Digest: versions[1].Digest}, bindings.Rollback)
	checked := &PipelineReference{VersionId: &versions[2].Id, Digest: versions[2].Digest}
	_, err = server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, checked, nil, true, false)
	require.ErrorContains(t, err, "pipeline_incompatible")
	require.NotContains(t, config["supported_protocols"], "stellwerk.pipeline/v1")
}

func TestPipelineChoicesRefuseUnqualifiedNativeProfiles(t *testing.T) {
	tests := []struct {
		name          string
		runnerType    string
		status        model.EnvironmentStatus
		config        map[string]any
		catalogue     map[string]any
		omitCatalogue bool
	}{
		{name: "absent catalogue", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, omitCatalogue: true},
		{name: "disabled catalogue", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, config: map[string]any{"native_execution": nil}},
		{name: "invalid catalogue digest", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, catalogue: map[string]any{"tools_digest": "invalid"}},
		{name: "legacy catalogue runtime", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, catalogue: map[string]any{"runtime_version": "stellwerk.runner.rollout/v1"}},
		{name: "missing pipeline protocol", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, config: map[string]any{"supported_protocols": []string{"stellwerk.pipeline.plan-apply/v2"}}},
		{name: "missing saved plan protocol", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, config: map[string]any{"supported_protocols": []string{"stellwerk.pipeline/v2"}}},
		{name: "model Native config legacy", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, config: map[string]any{"type": "kubernetes-agent", "supported_protocols": []string{"stellwerk.pipeline/v1"}}},
		{name: "model legacy config Native", runnerType: string(RunnerTypeKubernetesAgent), status: model.EnvironmentStatusActive, config: map[string]any{"supported_protocols": []string{"stellwerk.pipeline/v1", "stellwerk.pipeline/v2", "stellwerk.pipeline.plan-apply/v2"}}},
		{name: "legacy v2 only", runnerType: string(RunnerTypeKubernetesAgent), status: model.EnvironmentStatusActive, config: map[string]any{"type": "kubernetes-agent"}},
		{name: "inactive environment", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusDeleting},
		{name: "job override", runnerType: string(RunnerTypeNativeAgent), status: model.EnvironmentStatusActive, config: map[string]any{"job": map[string]any{"environment": map[string]string{"CUSTOM": "value"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, grant := pluginDeploymentBoundaryDatabase(t)
			environment := grant.Intent.Environments[0]
			config := nativeAgentTestConfiguration(t)
			config["capabilities"] = []string{"local.exec", "pipeline.recipe.v1"}
			catalogue := config["native_execution"].(map[string]any)
			for key, value := range test.catalogue {
				catalogue[key] = value
			}
			for key, value := range test.config {
				config[key] = value
			}
			if test.omitCatalogue {
				delete(config, "native_execution")
			}
			now := time.Now().UTC()
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = db.CreateRunner(t.Context(), tx, &model.Runner{OrgId: grant.OrgID, Id: "unqualified", RunnerType: test.runnerType,
				RunnerConfiguration: config, StateStorageType: "kubernetes", StateStorageConfiguration: map[string]any{}, CreatedAt: now, UpdatedAt: now})
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), "UPDATE envs SET runner_id=$1,status=$2 WHERE uuid=$3", "unqualified", test.status, environment.EnvironmentID)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			server := &Server{Database: db}
			selected := pluginDeploymentEnvironment{ProjectID: "project", EnvironmentID: "development", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID}
			choices, err := server.readPluginPipelineChoices(t.Context(), grant.OrgID, selected)
			require.NoError(t, err)
			require.Len(t, choices.Versions, 1)
			require.False(t, choices.Versions[0].Deploy.Compatible)
			require.False(t, choices.Versions[0].Rollback.Compatible)
			_, err = server.resolvePluginPipelineBindings(t.Context(), grant.OrgID, selected, nil, nil, false, true)
			require.ErrorContains(t, err, "pipeline_incompatible")
		})
	}
}
