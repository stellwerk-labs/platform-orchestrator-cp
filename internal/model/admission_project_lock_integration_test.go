//go:build integration

package model

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func waitAdmissionProjectBlocker(t *testing.T, db Databaser, holder, exclude int) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		return db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity
		 WHERE pid<>$2 AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))) LIMIT 1`, holder, exclude).Scan(&pid) == nil
	}, 5*time.Second, 5*time.Millisecond, "the real transaction must wait behind the owned row lock")
	return pid
}

func TestDestroyAdmissionLocksProjectBeforeEnvironment(t *testing.T) {
	db, _, request := admissionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	request.Mode, request.ModuleVersions, request.CarryForwardModuleVersions = "destroy", map[string]uuid.UUID{}, nil
	env, err := db.GetEnvironmentByUuid(ctx, nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	env, err = db.UpdateEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{Status: opt.Of(EnvironmentStatusDeleting), DeletionPolicy: &EnvironmentDeletionPolicy{}, UpdatedAt: time.Now()})
	require.NoError(t, err)
	project, err := db.GetProjectByUuid(ctx, nil, request.OrgID, request.ProjectUUID, GetModeDefault)
	require.NoError(t, err)
	receipt, err := AuthorizeDeploymentAdmission(ctx, db, request)
	require.NoError(t, err)
	var outboxBefore int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(ctx, `SELECT count(*) FROM pending_event_messages`).Scan(&outboxBefore))
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { cancel(); _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
	_, err = db.GetProjectByUuid(ctx, held, request.OrgID, request.ProjectUUID, GetModeForUpdate)
	require.NoError(t, err)
	_, err = db.GetEnvironmentByUuid(ctx, held, request.OrgID, request.EnvironmentUUID, GetModeForUpdate)
	require.NoError(t, err)
	rule, err := db.CreateModuleRule(ctx, held, request.OrgID, &DefinitionRule{OrgId: request.OrgID, DefinitionId: "alpha", ResourceType: "generic", ProjectId: opt.Of(project.Id), EnvId: opt.Of(env.Id), CreatedAt: time.Now()})
	require.NoError(t, err)
	type replayResult struct {
		receipt *DeploymentAdmissionReceipt
		err     error
	}
	replayed := make(chan replayResult, 1)
	go func() {
		value, err := AuthorizeDeploymentAdmission(ctx, db, request)
		replayed <- replayResult{value, err}
	}()
	replayPID := waitAdmissionProjectBlocker(t, db, holder, 0)
	deleted := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			deleted <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err = db.GetProjectByUuid(ctx, tx, request.OrgID, request.ProjectUUID, GetModeForUpdate); err == nil {
			err = db.DeleteProject(ctx, tx, request.OrgID, project.Id)
		}
		deleted <- err
	}()
	waitAdmissionProjectBlocker(t, db, holder, replayPID)
	require.NoError(t, held.Commit())
	var replay replayResult
	select {
	case replay = <-replayed:
	case <-ctx.Done():
		t.Fatal("Destroy replay did not finish")
	}
	var deleteErr error
	select {
	case deleteErr = <-deleted:
	case <-ctx.Done():
		t.Fatal("Project deletion did not finish")
	}
	require.NoError(t, replay.err)
	require.Equal(t, receipt, replay.receipt)
	_, conflict := IsErrConflict(deleteErr)
	require.True(t, conflict, "Project deletion must refuse its existing Environment, not deadlock: %v", deleteErr)
	current, err := db.GetEnvironmentByUuid(ctx, nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, env, current)
	currentProject, err := db.GetProjectByUuid(ctx, nil, request.OrgID, request.ProjectUUID, GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, project, currentProject)
	rules, _, err := db.ListModuleRules(ctx, nil, request.OrgID, "", 2, ListModuleRulesParams{})
	require.NoError(t, err)
	require.Equal(t, []DefinitionRule{*rule}, rules)
	epoch, err := ReadEnvironmentAdmissionEpoch(ctx, db, nil, request.OrgID, request.EnvironmentUUID)
	require.NoError(t, err)
	require.Equal(t, receipt.Epoch, epoch)
	var outboxAfter, guards int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(ctx, `SELECT count(*) FROM pending_event_messages`).Scan(&outboxAfter))
	require.Equal(t, outboxBefore, outboxAfter)
	require.NoError(t, db.(*databaser).DB.QueryRowContext(ctx, `SELECT count(*) FROM deployment_admission_guards`).Scan(&guards))
	require.Equal(t, 1, guards)
}
