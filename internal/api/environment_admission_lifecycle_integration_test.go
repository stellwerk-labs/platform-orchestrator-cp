//go:build integration

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func TestFreshAdmissionRequiresActiveEnvironment(t *testing.T) {
	for _, status := range []model.EnvironmentStatus{model.EnvironmentStatusDeleting, model.EnvironmentStatusDeleteFailed} {
		for _, mode := range []string{"deploy", "rollback"} {
			t.Run(string(status)+"/"+mode, func(t *testing.T) {
				db, grant := pluginDeploymentBoundaryDatabase(t)
				env, err := db.GetEnvironment(t.Context(), nil, grant.OrgID, "project", "development", model.GetModeDefault)
				require.NoError(t, err)
				_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &model.EnvironmentPatch{Status: opt.Of(status), UpdatedAt: time.Now()})
				require.NoError(t, err)
				admission := model.DeploymentAdmission{OrgID: env.OrgId, AdmissionID: uuid.New(), ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid, ActorID: grant.ActorID, RequestDigest: strings.Repeat("b", 64), Mode: mode, ModuleVersions: grant.Intent.TargetVersions, CarryForwardModuleVersions: grant.Intent.TargetVersions}
				receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
				_, conflict := model.IsErrConflict(err)
				require.True(t, conflict, "fresh admission must refuse non-active Environment: receipt=%+v error=%v", receipt, err)
				require.Nil(t, receipt)
				epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, env.OrgId, env.Uuid)
				require.NoError(t, err)
				require.Zero(t, epoch)
			})
		}
	}
}

func TestFreshAdmissionWaitsForCommittedDeletionIntent(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	env, err := db.GetEnvironment(ctx, nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	admission := model.DeploymentAdmission{OrgID: env.OrgId, AdmissionID: uuid.New(), ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid, ActorID: grant.ActorID, RequestDigest: strings.Repeat("b", 64), Mode: "deploy", ModuleVersions: grant.Intent.TargetVersions, CarryForwardModuleVersions: grant.Intent.TargetVersions}
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { cancel(); _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
	deleting, messages, err := DeleteEnvironmentAndCreateMessage(ctx, db, held, env.OrgId, env.ProjectId, env.Id, DeleteEnvOptions{})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	var outboxBefore int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT count(*) FROM pending_event_messages`).Scan(&outboxBefore))
	type result struct {
		receipt *model.DeploymentAdmissionReceipt
		err     error
	}
	admitted := make(chan result, 1)
	go func() {
		receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
		admitted <- result{receipt, err}
	}()
	var early result
	blocked := false
	require.Eventually(t, func() bool {
		select {
		case early = <-admitted:
			return true
		default:
		}
		probe, err := db.BeginTx(ctx, nil)
		if err != nil {
			return false
		}
		defer func() { _ = probe.Rollback() }()
		err = probe.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 5*time.Millisecond)
	require.True(t, blocked, "fresh admission returned before deletion committed: receipt=%+v error=%v", early.receipt, early.err)
	require.NoError(t, held.Commit())
	var completed result
	select {
	case completed = <-admitted:
	case <-ctx.Done():
		t.Fatal("fresh admission did not finish after deletion committed")
	}
	_, conflict := model.IsErrConflict(completed.err)
	require.True(t, conflict, "expected lifecycle conflict: %v", completed.err)
	require.Nil(t, completed.receipt)
	current, err := db.GetEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, deleting, current)
	epoch, err := model.ReadEnvironmentAdmissionEpoch(ctx, db, nil, env.OrgId, env.Uuid)
	require.NoError(t, err)
	require.Zero(t, epoch)
	check, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = check.Rollback() }()
	var guards, outboxAfter int
	require.NoError(t, check.QueryRowContext(ctx, `SELECT count(*) FROM deployment_admission_guards`).Scan(&guards))
	require.Zero(t, guards)
	require.NoError(t, check.QueryRowContext(ctx, `SELECT count(*) FROM pending_event_messages`).Scan(&outboxAfter))
	require.Equal(t, outboxBefore, outboxAfter)
}

func TestAcceptedAdmissionReplaysAfterDeletionIntent(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env, err := db.GetEnvironment(t.Context(), nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	admission := model.DeploymentAdmission{OrgID: env.OrgId, AdmissionID: uuid.New(), ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid, ActorID: grant.ActorID, RequestDigest: strings.Repeat("b", 64), Mode: "deploy", ModuleVersions: grant.Intent.TargetVersions, CarryForwardModuleVersions: grant.Intent.TargetVersions}
	receipt, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, messages, err := DeleteEnvironmentAndCreateMessage(t.Context(), db, tx, env.OrgId, env.ProjectId, env.Id, DeleteEnvOptions{})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.NoError(t, tx.Commit())
	replayed, err := model.AuthorizeDeploymentAdmission(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, receipt, replayed)
	epoch, err := model.ReadEnvironmentAdmissionEpoch(t.Context(), db, nil, env.OrgId, env.Uuid)
	require.NoError(t, err)
	require.Equal(t, receipt.Epoch, epoch)
}
