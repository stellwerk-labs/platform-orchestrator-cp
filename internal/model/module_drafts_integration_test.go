//go:build integration

package model

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestModuleDraftRevisionsAreEditableWithoutPublishing(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	actor := uuid.New()
	org, _, versions := externalOperationModules(t, db, actor)
	ctx := t.Context()
	original, err := db.GetModuleDefinitionVersion(ctx, nil, org, "alpha", versions[0].UUID.String())
	require.NoError(t, err)
	content := json.RawMessage(`{"semantic_version":"1.2.0","module_source":"git::https://example.com/source?ref=work-in-progress"}`)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err := db.CreateModuleDraft(ctx, tx, org, "alpha", content, actor, "Start candidate authoring")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, int64(1), draft.ResourceVersion)
	require.Equal(t, "open", draft.Status)
	_, err = db.GetModuleDefinitionVersion(ctx, nil, org, "alpha", draft.ID.String())
	_, missing := IsErrNotFound(err)
	require.True(t, missing, "an authoring Draft is not an executable Module Version")
	_, err = db.GetCoreModuleVersion(ctx, nil, org, "alpha", "1.2.0", GetModeDefault)
	_, missing = IsErrNotFound(err)
	require.True(t, missing, "saving a Draft must not reserve or publish its intended SemVer")
	changed := json.RawMessage(`{"semantic_version":"1.2.0","module_source":"git::https://example.com/source?ref=second-attempt"}`)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err = db.UpdateModuleDraft(ctx, tx, org, "alpha", draft.ID, 1, changed, actor, "Try another source")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, int64(2), draft.ResourceVersion)
	first, err := db.GetModuleDraftRevision(ctx, nil, org, "alpha", draft.ID, 1)
	require.NoError(t, err)
	require.JSONEq(t, string(content), string(first.Content))
	require.Equal(t, actor, first.Actor)
	_, err = db.GetModuleDraft(ctx, nil, "foreign", "alpha", draft.ID, GetModeDefault)
	_, missing = IsErrNotFound(err)
	require.True(t, missing)
	_, err = db.GetModuleDraft(ctx, nil, org, "bravo", draft.ID, GetModeDefault)
	_, missing = IsErrNotFound(err)
	require.True(t, missing)
	previous, err := db.GetModuleDefinitionVersion(ctx, nil, org, "alpha", versions[0].UUID.String())
	require.NoError(t, err)
	require.Equal(t, original.ModuleSourceCode, previous.ModuleSourceCode, "editing a Draft cannot rewrite published source")
	_, err = db.(*databaser).ExecContext(ctx, `UPDATE module_draft_revisions SET content='{}' WHERE draft_id=$1`, draft.ID)
	require.ErrorContains(t, err, "authoring history is immutable")
	provider := extensionMigrationProvider(t, db)
	_, err = provider.DownTo(ctx, 49)
	require.ErrorContains(t, err, "cannot downgrade while Draft authoring history exists")
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	closed, err := db.CloseModuleDraft(ctx, tx, org, "alpha", draft.ID, 2, nil, actor, "Discard the experiment")
	require.NoError(t, err)
	require.Equal(t, "discarded", closed.Status)
	_, err = db.CreateModuleDraft(ctx, tx, org, "alpha", content, actor, "Start again without changing the published version")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	page, err := db.ListModuleDrafts(ctx, nil, org, "alpha", uuid.Nil, 100)
	require.NoError(t, err)
	require.Len(t, page, 2)
}

func TestModuleDraftConcurrentEditsRequireExactRevision(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	actor := uuid.New()
	org, _, _ := externalOperationModules(t, db, actor)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err := db.CreateModuleDraft(ctx, tx, org, "alpha", json.RawMessage(`{"semantic_version":"1.2.0"}`), actor, "Start draft")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	results := make(chan error, 5)
	var writers sync.WaitGroup
	for range 5 {
		writers.Go(func() {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			_, err = db.UpdateModuleDraft(ctx, tx, org, "alpha", draft.ID, 1,
				json.RawMessage(`{"semantic_version":"1.2.0","edit":"`+uuid.NewString()+`"}`), actor, "Save an exact editor revision")
			if err == nil {
				err = tx.Commit()
			}
			results <- err
		})
	}
	writers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		_, conflict := IsErrConflict(err)
		require.True(t, conflict, "unexpected error: %v", err)
	}
	require.Equal(t, 1, successes)
	current, err := db.GetModuleDraft(ctx, nil, org, "alpha", draft.ID, GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, int64(2), current.ResourceVersion)
	_, err = db.GetModuleDraftRevision(ctx, nil, org, "alpha", draft.ID, 3)
	_, missing := IsErrNotFound(err)
	require.True(t, missing, "rejected edits must not leave partial history")
}
