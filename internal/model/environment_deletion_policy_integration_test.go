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

func TestDeletionPolicyDestroyAdmissionLocksCurrentPolicyIncludingReplay(t *testing.T) {
	db, _, request := admissionFixture(t)
	request.Mode, request.ModuleVersions, request.CarryForwardModuleVersions = "destroy", map[string]uuid.UUID{}, nil
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{Status: opt.Of(EnvironmentStatusDeleting), UpdatedAt: time.Now()})
	require.NoError(t, err)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "non-force deletion policy", "legacy NULL is not a normal Destroy request")
	_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{DeletionPolicy: &EnvironmentDeletionPolicy{}, UpdatedAt: time.Now()})
	require.NoError(t, err)
	held, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.GetEnvironmentByUuid(t.Context(), held, request.OrgID, request.EnvironmentUUID, GetModeForUpdate)
	require.NoError(t, err)
	blocked, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	_, err = AuthorizeDeploymentAdmission(blocked, db, request)
	require.Error(t, err)
	require.Error(t, blocked.Err())
	cancel()
	require.NoError(t, held.Rollback())
	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	replay, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.GetEnvironmentByUuid(t.Context(), tx, request.OrgID, request.EnvironmentUUID, GetModeForUpdate)
	require.NoError(t, err)
	_, err = db.UpdateEnvironment(t.Context(), tx, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{DeletionPolicy: &EnvironmentDeletionPolicy{Force: true}, UpdatedAt: time.Now()})
	require.NoError(t, err)
	for _, candidate := range []DeploymentAdmission{request} {
		blocked, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		_, err = AuthorizeDeploymentAdmission(blocked, db, candidate)
		require.Error(t, err)
		require.Error(t, blocked.Err(), "admission must wait for the actual Environment row lock")
		cancel()
	}
	require.NoError(t, tx.Commit())
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "non-force deletion policy", "old admission replay cannot bypass force escalation")
	// Existing accepted work may still settle after force; policy is no cancellation receipt.
	_, err = SettleDeploymentAdmission(t.Context(), db, env.OrgId, receipt.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)
	request.AdmissionID = uuid.New()
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.ErrorContains(t, err, "non-force deletion policy")
}

func TestDeletionPolicyMonotoneProjectionAndStaleStatus(t *testing.T) {
	db, _, request := admissionFixture(t)
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, env.DeletionForce)
	require.Nil(t, env.DeletionDeleteRules)
	for _, policy := range []EnvironmentDeletionPolicy{{}, {DeleteRules: true}, {Force: true}, {}} {
		env, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{DeletionPolicy: &policy, Status: opt.Of(EnvironmentStatusDeleting), UpdatedAt: time.Now()})
		require.NoError(t, err)
	}
	for _, status := range []EnvironmentStatus{EnvironmentStatusActive, EnvironmentStatusDeleteFailed} {
		_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{Status: opt.Of(status), UpdatedAt: time.Now()})
		require.ErrorContains(t, err, "stale status")
	}
	verify := func(value *Environment) {
		t.Helper()
		require.NotNil(t, value.DeletionForce)
		require.True(t, *value.DeletionForce)
		require.NotNil(t, value.DeletionDeleteRules)
		require.True(t, *value.DeletionDeleteRules)
	}
	byName, err := db.GetEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, GetModeDefault)
	require.NoError(t, err)
	verify(byName)
	byUUID, err := db.GetEnvironmentByUuid(t.Context(), nil, env.OrgId, env.Uuid, GetModeDefault)
	require.NoError(t, err)
	verify(byUUID)
	page, _, err := db.ListEnvironments(t.Context(), nil, env.OrgId, env.ProjectId, "", 20, ListEnvironmentsParams{})
	require.NoError(t, err)
	require.Len(t, page, 1)
	verify(&page[0])
	page, _, err = db.ListEnvironmentsInOrg(t.Context(), nil, env.OrgId, "", 20, ListEnvironmentsParams{})
	require.NoError(t, err)
	require.Len(t, page, 1)
	verify(&page[0])
	page, _, err = db.ListEnvironmentsByProjectUuid(t.Context(), nil, env.OrgId, env.ProjectUuid, "", 20)
	require.NoError(t, err)
	require.Len(t, page, 1)
	verify(&page[0])
	runnerTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateRunner(t.Context(), runnerTx, &Runner{OrgId: env.OrgId, Id: "deletion-fixture", RunnerType: "serverless-ecs", RunnerConfiguration: map[string]any{}, StateStorageType: "s3", StateStorageConfiguration: map[string]any{}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, runnerTx.Commit())
	_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{RunnerId: opt.Of("deletion-fixture"), UpdatedAt: time.Now()})
	require.NoError(t, err)
	page, _, err = db.ListEnvironmentsByRunnerId(t.Context(), nil, env.OrgId, "deletion-fixture", "", 20)
	require.NoError(t, err)
	require.Len(t, page, 1)
	verify(&page[0])
	// Exercise the nullable-pair DB constraint independently of application helpers.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE envs SET deletion_force=NULL WHERE uuid=$1`, env.Uuid)
	require.ErrorContains(t, err, "envs_deletion_policy_pair")
	require.NoError(t, tx.Rollback())
}

func TestDeletionPolicyMigrationPreservesUnknownAndRefusesPolicyLoss(t *testing.T) {
	db, _, request := admissionFixture(t)
	provider := extensionMigrationProvider(t, db)
	_, err := provider.DownTo(t.Context(), 53)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, env.DeletionForce)
	require.Nil(t, env.DeletionDeleteRules)
	_, err = db.UpdateEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{DeletionPolicy: &EnvironmentDeletionPolicy{Force: true}, Status: opt.Of(EnvironmentStatusDeleting), UpdatedAt: time.Now()})
	require.NoError(t, err)
	_, err = provider.DownTo(t.Context(), 53)
	require.ErrorContains(t, err, "cannot discard accepted Environment deletion policy")
	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(54), version)
}

func TestDeletionPolicyWaitingStaleUpdateCannotUndoCommittedForce(t *testing.T) {
	db, _, request := admissionFixture(t)
	env, err := db.GetEnvironmentByUuid(t.Context(), nil, request.OrgID, request.EnvironmentUUID, GetModeDefault)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.UpdateEnvironment(t.Context(), tx, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{DeletionPolicy: &EnvironmentDeletionPolicy{Force: true}, Status: opt.Of(EnvironmentStatusDeleting), UpdatedAt: time.Now()})
	require.NoError(t, err)
	staleTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = staleTx.Rollback() }()
	var pid int
	require.NoError(t, staleTx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	result := make(chan error, 1)
	go func() {
		_, err := db.UpdateEnvironment(t.Context(), staleTx, env.OrgId, env.ProjectId, env.Id, &EnvironmentPatch{Status: opt.Of(EnvironmentStatusDeleteFailed), UpdatedAt: time.Now()})
		result <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 10*time.Millisecond, "stale worker must actually wait on the Environment row")
	require.NoError(t, tx.Commit())
	select {
	case err := <-result:
		require.ErrorContains(t, err, "stale status")
	case <-time.After(3 * time.Second):
		t.Fatal("stale update did not reconcile after force commit")
	}
	require.NoError(t, staleTx.Rollback())

}
