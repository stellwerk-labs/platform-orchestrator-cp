//go:build integration

package model

import (
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func rollbackPolicyVersions(t *testing.T, db Databaser, project *Project, actor uuid.UUID) (p.PublishedPipelineVersion, p.PublishedPipelineVersion) {
	t.Helper()
	draft := pipelineTestDraft(t, db, project, actor)
	forward := pipelineTestPublish(t, db, project, actor, draft, "forward")
	changed, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return SavePipelineDraft(t.Context(), tx, scope, draft.Id, actor, p.PipelineDraftWrite{ExpectedResourceVersion: draft.ResourceVersion, Name: "Rollback", Definition: p.PipelineRecipe{Strategy: p.Apply, Steps: []p.PipelineRecipeStep{}}})
	})
	require.NoError(t, err)
	return forward, pipelineTestPublish(t, db, project, actor, changed, "rollback")
}

func writeRollbackPolicy(t *testing.T, db Databaser, project *Project, actor uuid.UUID, body p.PipelinePolicyWrite) (p.PipelinePolicy, error) {
	t.Helper()
	return pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, body)
	})
}

func TestRollbackDefaultsPreserveOldWritersAndRequireExplicitClear(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	forward, rollback := rollbackPolicyVersions(t, db, project, actor)
	body := p.PipelinePolicyWrite{Enabled: true, DefaultVersionId: &forward.Id, AllowedVersionIds: []uuid.UUID{forward.Id, rollback.Id}, RollbackDefaults: &p.PipelineRollbackDefaults{DefaultVersionId: &rollback.Id}}
	policy, err := writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	require.Equal(t, rollback.Id, *policy.RollbackDefaults.DefaultVersionId)
	body.ExpectedResourceVersion, body.RollbackDefaults = policy.ResourceVersion, nil
	policy, err = writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	require.Equal(t, rollback.Id, *policy.RollbackDefaults.DefaultVersionId)
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "reload")
	body.ExpectedResourceVersion = policy.ResourceVersion
	body.AllowedVersionIds = []uuid.UUID{forward.Id}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "rollback default requires an allowed")
	body.Enabled, body.DefaultVersionId, body.AllowedVersionIds = false, nil, []uuid.UUID{}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "clear rollback defaults explicitly")
	body.RollbackDefaults = &p.PipelineRollbackDefaults{}
	policy, err = writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	require.False(t, policy.Enabled)
	require.NotNil(t, policy.RollbackDefaults)
	require.Nil(t, policy.RollbackDefaults.DefaultVersionId)
	require.Empty(t, policy.RollbackDefaults.EnvironmentDefaultVersionIds)
}

func TestRollbackEnvironmentDefaultsValidateScopeAndCanonicalIdentity(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	forward, rollback := rollbackPolicyVersions(t, db, project, actor)
	org, err := db.GetOrg(t.Context(), nil, project.OrgId)
	require.NoError(t, err)
	envType, err := db.CreateEnvironmentType(t.Context(), nil, &EnvType{OrgId: project.OrgId, OrgUuid: org.Uuid, Id: "rollback", DisplayName: "Rollback"})
	require.NoError(t, err)
	env, err := db.CreateEnvironment(t.Context(), nil, &Environment{OrgId: project.OrgId, OrgUuid: org.Uuid, ProjectId: project.Id, ProjectUuid: project.Uuid, Id: "rollback", EnvTypeId: envType.Id, EnvTypeUuid: envType.Uuid, Status: EnvironmentStatusActive})
	require.NoError(t, err)
	body := p.PipelinePolicyWrite{Enabled: true, DefaultVersionId: &forward.Id, AllowedVersionIds: []uuid.UUID{forward.Id, rollback.Id}, RollbackDefaults: &p.PipelineRollbackDefaults{DefaultVersionId: &rollback.Id, EnvironmentDefaultVersionIds: map[string]uuid.UUID{env.Uuid.String(): rollback.Id}}}
	policy, err := writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	require.Equal(t, body.RollbackDefaults.EnvironmentDefaultVersionIds, policy.RollbackDefaults.EnvironmentDefaultVersionIds)
	body.ExpectedResourceVersion = policy.ResourceVersion
	body.RollbackDefaults.EnvironmentDefaultVersionIds = map[string]uuid.UUID{env.Uuid.String(): rollback.Id, "urn:uuid:" + env.Uuid.String(): forward.Id}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "canonical immutable")
	body.RollbackDefaults.EnvironmentDefaultVersionIds = map[string]uuid.UUID{uuid.NewString(): rollback.Id}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "does not belong")
	body.RollbackDefaults.EnvironmentDefaultVersionIds = map[string]uuid.UUID{env.Uuid.String(): uuid.New()}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.ErrorContains(t, err, "allowed version")
}

func TestRollbackDefaultMigrationRefusesDataLoss(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	forward, rollback := rollbackPolicyVersions(t, db, project, actor)
	body := p.PipelinePolicyWrite{Enabled: true, DefaultVersionId: &forward.Id, AllowedVersionIds: []uuid.UUID{forward.Id, rollback.Id}, RollbackDefaults: &p.PipelineRollbackDefaults{DefaultVersionId: &rollback.Id}}
	policy, err := writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	sqlDB := db.(*databaser).DB
	require.ErrorContains(t, goose.DownToContext(t.Context(), sqlDB, "migrations", 60), "cannot discard rollback")
	body.ExpectedResourceVersion, body.RollbackDefaults = policy.ResourceVersion, &p.PipelineRollbackDefaults{}
	_, err = writeRollbackPolicy(t, db, project, actor, body)
	require.NoError(t, err)
	require.NoError(t, goose.DownToContext(t.Context(), sqlDB, "migrations", 60))
	var id uuid.UUID
	require.NoError(t, sqlDB.QueryRowContext(t.Context(), "SELECT default_version_id FROM project_pipeline_policy WHERE project_uuid=$1", project.Uuid).Scan(&id))
	require.Equal(t, forward.Id, id)
}
