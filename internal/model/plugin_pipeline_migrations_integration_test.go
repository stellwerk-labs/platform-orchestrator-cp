//go:build integration

package model

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestPluginPipelineMigrationHistories(t *testing.T) {
	for _, history := range []string{"empty", "customer57", "fixture58"} {
		t.Run(history, func(t *testing.T) {
			dsn := isolatedSchema(t)
			core, err := NewDatabaser(t.Context(), zap.NewNop(), dsn)
			require.NoError(t, err)
			defer func() {
				require.NoError(t, core.Close())
			}()
			sqlDB := schemaConnection(t, dsn)
			require.NoError(t, goose.DownToContext(t.Context(), sqlDB, "migrations", 56))
			defer goose.SetBaseFS(embedMigrations)
			old := fstest.MapFS{}
			paths, err := fs.Glob(embedMigrations, "migrations/*")
			require.NoError(t, err)
			for _, path := range paths {
				if path >= "migrations/000057" {
					continue
				}
				raw, err := embedMigrations.ReadFile(path)
				require.NoError(t, err)
				old[path] = &fstest.MapFile{Data: raw}
			}
			if history == "customer57" {
				raw, err := embedMigrations.ReadFile("migrations/000057_observation_deletion_responses.sql")
				require.NoError(t, err)
				old["migrations/000057_observation_deletion_responses.sql"] = &fstest.MapFile{Data: raw}
			}
			var draftID uuid.UUID
			if history == "fixture58" {
				for _, pair := range [][2]string{{"000058_project_pipelines.sql", "000057_project_pipelines.sql"}, {"000059_pipeline_draft_archive.sql", "000058_pipeline_draft_archive.sql"}} {
					raw, err := embedMigrations.ReadFile("migrations/" + pair[0])
					require.NoError(t, err)
					old["migrations/"+pair[1]] = &fstest.MapFile{Data: raw}
				}
			}
			goose.SetBaseFS(old)
			require.NoError(t, goose.UpContext(t.Context(), sqlDB, "migrations"))
			if history == "fixture58" {
				actor := uuid.New()
				org, _, _ := externalOperationModules(t, core, actor)
				organization, err := core.GetOrg(t.Context(), nil, org)
				require.NoError(t, err)
				project, err := core.CreateProject(t.Context(), nil, &Project{Id: "preserved", DisplayName: "Preserved", OrgId: org, OrgUuid: organization.Uuid, Status: ProjectStatusActive})
				require.NoError(t, err)
				draft := pipelineTestDraft(t, core, project, actor)
				draftID = draft.Id
				_, err = pipelineTestTx(t, core, project, func(tx Tx, scope *Project) (bool, error) {
					return true, ArchivePipelineDraft(t.Context(), tx, scope, draft.Id, actor, draft.ResourceVersion)
				})
				require.NoError(t, err)
			}
			goose.SetBaseFS(embedMigrations)
			require.NoError(t, goose.UpContext(t.Context(), sqlDB, "migrations"))
			var count int
			require.NoError(t, sqlDB.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='project_pipeline_policy' AND column_name='environment_default_version_ids'`).Scan(&count))
			require.Equal(t, 1, count)
			require.NoError(t, sqlDB.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='project_pipeline_policy' AND column_name='rollback_defaults'`).Scan(&count))
			require.Equal(t, 1, count)
			if draftID != uuid.Nil {
				var archived bool
				require.NoError(t, sqlDB.QueryRow(`SELECT archived_at IS NOT NULL FROM project_pipeline_drafts WHERE id=$1`, draftID).Scan(&archived))
				require.True(t, archived)
				require.NoError(t, sqlDB.QueryRow(`SELECT count(*) FROM project_pipeline_history WHERE subject=$1`, draftID).Scan(&count))
				require.Positive(t, count)
			}
			versions := appliedMigrations(t, sqlDB)
			require.Equal(t, int64(65), versions[len(versions)-1], fmt.Sprint(versions))
		})
	}
}
func TestEnvironmentPipelineDefaultPolicyCAS(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	version := pipelineTestPublish(t, db, project, actor, draft, "publish")
	organization, err := db.GetOrg(t.Context(), nil, project.OrgId)
	require.NoError(t, err)
	envType, err := db.CreateEnvironmentType(t.Context(), nil, &EnvType{OrgId: project.OrgId, OrgUuid: organization.Uuid, Id: "pipeline", DisplayName: "Pipeline"})
	require.NoError(t, err)
	env, err := db.CreateEnvironment(t.Context(), nil, &Environment{OrgId: project.OrgId, OrgUuid: organization.Uuid, ProjectId: project.Id, ProjectUuid: project.Uuid, Id: "pipeline", EnvTypeId: envType.Id, EnvTypeUuid: envType.Uuid, Status: EnvironmentStatusActive})
	require.NoError(t, err)
	body := p.PipelinePolicyWrite{Enabled: true, DefaultVersionId: &version.Id, AllowedVersionIds: []uuid.UUID{version.Id}, EnvironmentDefaultVersionIds: map[string]uuid.UUID{env.Uuid.String(): version.Id}}
	policy, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, body)
	})
	require.NoError(t, err)
	require.Equal(t, body.EnvironmentDefaultVersionIds, policy.EnvironmentDefaultVersionIds)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, body)
	})
	require.ErrorContains(t, err, "reload")
	body.ExpectedResourceVersion = policy.ResourceVersion
	body.EnvironmentDefaultVersionIds = map[string]uuid.UUID{uuid.NewString(): version.Id}
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, body)
	})
	require.ErrorContains(t, err, "does not belong")
}
