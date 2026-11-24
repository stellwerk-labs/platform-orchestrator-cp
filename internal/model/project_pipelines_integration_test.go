//go:build integration

package model

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func pipelineFixture(t *testing.T) (Databaser, *Project, uuid.UUID) {
	t.Helper()
	db := externalOperationDatabase(t)
	actor := uuid.New()
	org, _, _ := externalOperationModules(t, db, actor)
	organization, err := db.GetOrg(t.Context(), nil, org)
	require.NoError(t, err)
	project, err := db.CreateProject(t.Context(), nil, &Project{Id: "pipelines", DisplayName: "Pipeline tests", OrgId: org, OrgUuid: organization.Uuid, Status: ProjectStatusActive})
	require.NoError(t, err)
	return db, project, actor
}
func pipelineTestTx[T any](t *testing.T, db Databaser, project *Project, action func(Tx, *Project) (T, error)) (T, error) {
	t.Helper()
	var zero T
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := LockPipelineProject(t.Context(), db, tx, project.OrgId, project.Id)
	if err != nil {
		return zero, err
	}
	value, err := action(tx, locked)
	if err != nil {
		return zero, err
	}
	return value, tx.Commit()
}
func pipelineTestDraft(t *testing.T, db Databaser, project *Project, actor uuid.UUID) p.PipelineDraft {
	t.Helper()
	draft, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return CreatePipelineDraft(t.Context(), tx, scope, actor, p.PipelineDraftCreate{Slug: "reviewed", Name: "Reviewed deployment", Definition: p.PipelineRecipe{Strategy: p.Reviewed, Steps: []p.PipelineRecipeStep{}}})
	})
	require.NoError(t, err)
	return draft
}
func pipelineTestPublish(t *testing.T, db Databaser, project *Project, actor uuid.UUID, draft p.PipelineDraft, key string) p.PublishedPipelineVersion {
	t.Helper()
	version, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return PublishPipeline(t.Context(), tx, scope, draft.Id, actor, p.PipelinePublish{ExpectedResourceVersion: draft.ResourceVersion, RequestKey: key})
	})
	require.NoError(t, err)
	return version
}
func TestProjectPipelineDraftCASAndImmutablePublication(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	version := pipelineTestPublish(t, db, project, actor, draft, "publish-first")
	replay := pipelineTestPublish(t, db, project, actor, draft, "publish-first")
	require.Equal(t, version, replay)
	_, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return PublishPipeline(t.Context(), tx, scope, draft.Id, uuid.New(), p.PipelinePublish{ExpectedResourceVersion: 1, RequestKey: "publish-first"})
	})
	require.ErrorContains(t, err, "another command")
	changed, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return SavePipelineDraft(t.Context(), tx, scope, draft.Id, actor, p.PipelineDraftWrite{ExpectedResourceVersion: 1, Name: "Changed recipe", Definition: p.PipelineRecipe{Strategy: p.Apply, Steps: []p.PipelineRecipeStep{}}})
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), changed.ResourceVersion)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return SavePipelineDraft(t.Context(), tx, scope, draft.Id, actor, p.PipelineDraftWrite{ExpectedResourceVersion: 1, Name: "Lost update", Definition: draft.Definition})
	})
	require.ErrorContains(t, err, "changed")
	replay = pipelineTestPublish(t, db, project, actor, draft, "publish-first")
	require.Equal(t, version, replay, "accepted publication recovers before mutable draft lookup")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, _ *Project) (bool, error) {
		_, err := tx.ExecContext(t.Context(), `UPDATE project_pipeline_versions SET name='rewritten' WHERE id=$1`, version.Id)
		return false, err
	})
	require.ErrorContains(t, err, "immutable")
	other := *project
	other.Uuid = uuid.New()
	_, err = pipelineTestTx(t, db, project, func(tx Tx, _ *Project) (p.PublishedPipelineVersion, error) {
		return ReadPublishedPipeline(t.Context(), tx, &other, version.Id)
	})
	_, missing := IsErrNotFound(err)
	require.True(t, missing)
}
func TestProjectPipelineConcurrentPublicationReturnsOneVersion(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	var wg sync.WaitGroup
	results := make(chan p.PublishedPipelineVersion, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			value, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
				return PublishPipeline(t.Context(), tx, scope, draft.Id, actor, p.PipelinePublish{ExpectedResourceVersion: 1, RequestKey: "same-publication"})
			})
			if err != nil {
				failures <- err
			} else {
				results <- value
			}
		})
	}
	wg.Wait()
	close(failures)
	close(results)
	for err := range failures {
		require.NoError(t, err)
	}
	id := uuid.Nil
	count := 0
	for value := range results {
		if id == uuid.Nil {
			id = value.Id
		}
		require.Equal(t, id, value.Id)
		count++
	}
	require.Equal(t, 4, count)
}
func TestProjectPipelineAllowedPolicyRetirementAndDefaultGuards(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	version := pipelineTestPublish(t, db, project, actor, draft, "version")
	policy, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, p.PipelinePolicyWrite{Enabled: true, AllowedVersionIds: []uuid.UUID{version.Id}, DefaultVersionId: &version.Id})
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), policy.ResourceVersion)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (bool, error) {
		return true, CheckPipelineAdmission(t.Context(), tx, scope, nil)
	})
	require.ErrorContains(t, err, "requires a published pipeline")
	reference := &PipelineAdmissionReference{PipelineID: version.PipelineId, VersionID: version.Id, Version: version.Version, Digest: version.Digest}
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (bool, error) {
		return true, CheckPipelineAdmission(t.Context(), tx, scope, reference)
	})
	require.NoError(t, err)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return RetirePipeline(t.Context(), tx, scope, version.Id, actor)
	})
	require.ErrorContains(t, err, "remove this version")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, p.PipelinePolicyWrite{ExpectedResourceVersion: 0, Enabled: false, AllowedVersionIds: []uuid.UUID{}})
	})
	require.ErrorContains(t, err, "policy changed")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, p.PipelinePolicyWrite{ExpectedResourceVersion: 1, Enabled: false, AllowedVersionIds: []uuid.UUID{}})
	})
	require.NoError(t, err)
	retired, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return RetirePipeline(t.Context(), tx, scope, version.Id, actor)
	})
	require.NoError(t, err)
	require.NotNil(t, retired.RetiredAt)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (bool, error) {
		return true, CheckPipelineAdmission(t.Context(), tx, scope, reference)
	})
	require.ErrorContains(t, err, "retired")
}

func TestProjectPipelinePolicyGuardsOrdinaryCoreAdmission(t *testing.T) {
	db, _, request := admissionFixture(t)
	project, err := db.GetProjectByUuid(t.Context(), nil, request.OrgID, request.ProjectUUID, GetModeDefault)
	require.NoError(t, err)
	draft := pipelineTestDraft(t, db, project, request.ActorID)
	version := pipelineTestPublish(t, db, project, request.ActorID, draft, "guard-version")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, request.ActorID, p.PipelinePolicyWrite{Enabled: true, AllowedVersionIds: []uuid.UUID{version.Id}, DefaultVersionId: &version.Id})
	})
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "requires a published pipeline")
	request.Pipeline = &PipelineAdmissionReference{PipelineID: version.PipelineId, VersionID: version.Id, Version: version.Version, Digest: version.Digest}
	wrong := request
	wrong.Pipeline = &PipelineAdmissionReference{PipelineID: version.PipelineId, VersionID: version.Id, Version: version.Version, Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, wrong)
	require.ErrorContains(t, err, "provenance")
	unsupported := request
	unsupported.Mode = "destroy"
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, unsupported)
	require.Error(t, err, "Destroy remains outside qualified published pipeline operations")
	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	require.Positive(t, receipt.Epoch)
	replay, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
}

func TestProjectPipelineHistoryRefusesPhysicalProjectDeletionClearly(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	err := db.DeleteProject(t.Context(), nil, project.OrgId, project.Id)
	conflict, ok := IsErrConflict(err)
	require.True(t, ok)
	require.Equal(t, "project_pipeline_history_retained", conflict.Code)
	require.Contains(t, conflict.Message, "archival")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return ReadPipelineDraft(t.Context(), tx, scope, draft.Id)
	})
	require.NoError(t, err, "deletion refusal retains the project and authoring evidence")
}

func TestProjectPipelineArchiveRetainsHistoryAndReusesOnlySlug(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	version := pipelineTestPublish(t, db, project, actor, draft, "before-archive")
	archive := func(expected int64) error {
		_, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (bool, error) {
			return true, ArchivePipelineDraft(t.Context(), tx, scope, draft.Id, actor, expected)
		})
		return err
	}
	require.ErrorContains(t, archive(2), "changed")
	require.ErrorContains(t, archive(1), "retire all")
	policy, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, p.PipelinePolicyWrite{ExpectedResourceVersion: 0, Enabled: true, DefaultVersionId: &version.Id, AllowedVersionIds: []uuid.UUID{version.Id}})
	})
	require.NoError(t, err)
	require.ErrorContains(t, archive(1), "policy")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelinePolicy, error) {
		return SetPipelinePolicy(t.Context(), tx, scope, actor, p.PipelinePolicyWrite{ExpectedResourceVersion: policy.ResourceVersion, Enabled: false, AllowedVersionIds: []uuid.UUID{}})
	})
	require.NoError(t, err)
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return RetirePipeline(t.Context(), tx, scope, version.Id, actor)
	})
	require.NoError(t, err)
	require.NoError(t, archive(1))
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return ReadPipelineDraft(t.Context(), tx, scope, draft.Id)
	})
	require.ErrorContains(t, err, "not found")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return SavePipelineDraft(t.Context(), tx, scope, draft.Id, actor, p.PipelineDraftWrite{ExpectedResourceVersion: 1, Name: "Resurrect", Definition: draft.Definition})
	})
	require.ErrorContains(t, err, "not found")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return PublishPipeline(t.Context(), tx, scope, draft.Id, actor, p.PipelinePublish{ExpectedResourceVersion: 1, RequestKey: "new-after-archive"})
	})
	require.ErrorContains(t, err, "not found")
	replay := pipelineTestPublish(t, db, project, actor, draft, "before-archive")
	require.Equal(t, version.Id, replay.Id)
	replacement := pipelineTestDraft(t, db, project, actor)
	require.Equal(t, draft.Slug, replacement.Slug)
	require.NotEqual(t, draft.Id, replacement.Id)
	retained, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PublishedPipelineVersion, error) {
		return ReadPublishedPipeline(t.Context(), tx, scope, version.Id)
	})
	require.NoError(t, err)
	require.Equal(t, draft.Id, retained.PipelineId)
	require.NotNil(t, retained.RetiredAt)
	catalogue, err := pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineCatalogue, error) {
		return ReadPipelineCatalogue(t.Context(), tx, scope)
	})
	require.NoError(t, err)
	require.Len(t, catalogue.Drafts, 1)
	require.Equal(t, replacement.Id, catalogue.Drafts[0].Id)
}

func TestProjectPipelineArchiveCannotCrossProjectScope(t *testing.T) {
	db, project, actor := pipelineFixture(t)
	draft := pipelineTestDraft(t, db, project, actor)
	foreign := *project
	foreign.Uuid = uuid.New()
	_, err := pipelineTestTx(t, db, project, func(tx Tx, _ *Project) (bool, error) {
		return true, ArchivePipelineDraft(t.Context(), tx, &foreign, draft.Id, actor, 1)
	})
	require.ErrorContains(t, err, "not found")
	_, err = pipelineTestTx(t, db, project, func(tx Tx, scope *Project) (p.PipelineDraft, error) {
		return ReadPipelineDraft(t.Context(), tx, scope, draft.Id)
	})
	require.NoError(t, err)
}
