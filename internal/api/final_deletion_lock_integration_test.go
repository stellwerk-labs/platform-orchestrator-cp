//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genevents"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func finalDeletionRouter(t *testing.T, db model.Databaser) *echo.Echo {
	t.Helper()
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "final-deletion-lock", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	return router
}

func finalDeletionRequest(ctx context.Context, router *echo.Echo, env *model.Environment, expected *uuid.UUID) *httptest.ResponseRecorder {
	path := "/internal/orgs/" + env.OrgId + "/projects/" + env.ProjectId + "/envs/" + env.Id + "/actions/force-delete?deleteRules=true"
	if expected != nil {
		path += "&expectedEnvUuid=" + expected.String()
	}
	request := httptest.NewRequest(http.MethodPost, path, nil).WithContext(ctx)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func waitFinalDeletionLock(t *testing.T, db model.Databaser, holder, exclude int) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return false
		}
		defer func() { _ = tx.Rollback() }()
		return tx.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE pid<>$2
		 AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))) LIMIT 1`, holder, exclude).Scan(&pid) == nil
	}, 5*time.Second, 5*time.Millisecond, "the actual request must be queued behind the owned PostgreSQL lock")
	return pid
}

func finalDeletionOutboxCount(t *testing.T, db model.Databaser) int {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages WHERE routing_key=$1`, genevents.IoPlatformOrchestratorEnvironmentDeleted).Scan(&count))
	return count
}

func TestFinalDeletionSerializesBeforeRowLockAgainstDestroyReplay(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	env, err := db.GetEnvironment(ctx, nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	env, err = db.UpdateEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, &model.EnvironmentPatch{Status: opt.Of(model.EnvironmentStatusDeleting), DeletionPolicy: &model.EnvironmentDeletionPolicy{}, UpdatedAt: time.Now()})
	require.NoError(t, err)
	admission := model.DeploymentAdmission{OrgID: env.OrgId, AdmissionID: uuid.New(), ProjectUUID: env.ProjectUuid, EnvironmentUUID: env.Uuid, ActorID: grant.ActorID, RequestDigest: strings.Repeat("b", 64), Mode: "destroy", ModuleVersions: map[string]uuid.UUID{}}
	receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
	require.NoError(t, err)
	before := finalDeletionOutboxCount(t, db)
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { cancel(); _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid(),pg_advisory_xact_lock(hashtextextended($1,3))`, "external-dispatch-environment:"+env.Uuid.String()).Scan(&holder, new(any)))
	_, err = db.GetEnvironment(ctx, held, env.OrgId, env.ProjectId, env.Id, model.GetModeForUpdate)
	require.NoError(t, err)
	router := finalDeletionRouter(t, db)
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- finalDeletionRequest(ctx, router, env, &env.Uuid) }()
	deletionPID := waitFinalDeletionLock(t, db, holder, 0)
	type replayResult struct {
		receipt *model.DeploymentAdmissionReceipt
		err     error
	}
	replayed := make(chan replayResult, 1)
	go func() {
		value, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
		replayed <- replayResult{value, err}
	}()
	waitFinalDeletionLock(t, db, holder, deletionPID)
	require.NoError(t, held.Commit())
	select {
	case response := <-deleted:
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	case <-ctx.Done():
		t.Fatal("final deletion did not finish")
	}
	select {
	case result := <-replayed:
		require.NoError(t, result.err)
		require.Equal(t, receipt, result.receipt)
	case <-ctx.Done():
		t.Fatal("exact Destroy admission replay did not finish")
	}
	current, err := db.GetEnvironment(ctx, nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, env, current)
	require.Equal(t, before, finalDeletionOutboxCount(t, db))
	epoch, err := model.ReadEnvironmentAdmissionEpoch(ctx, db, nil, env.OrgId, env.Uuid)
	require.NoError(t, err)
	require.Equal(t, receipt.Epoch, epoch)
}

func TestFinalDeletionRefusesRecreatedLifecycleWhileWaiting(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	env, err := db.GetEnvironment(t.Context(), nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	before := finalDeletionOutboxCount(t, db)
	held, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { cancel(); _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(t.Context(), `SELECT pg_backend_pid(),pg_advisory_xact_lock(hashtextextended($1,3))`, "external-dispatch-environment:"+env.Uuid.String()).Scan(&holder, new(any)))
	router := finalDeletionRouter(t, db)
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- finalDeletionRequest(ctx, router, env, nil) }()
	waitFinalDeletionLock(t, db, holder, 0)
	writerCtx, writerCancel := context.WithTimeout(ctx, 3*time.Second)
	defer writerCancel()
	writer, err := db.BeginTx(writerCtx, nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	// A legacy/raw writer does not join the new final-deletion coordination.
	require.NoError(t, db.DeleteEnvironment(writerCtx, writer, env.OrgId, env.ProjectId, env.Id))
	replacement := *env
	replacement.Uuid = uuid.Nil
	created, err := db.CreateEnvironment(writerCtx, writer, &replacement)
	require.NoError(t, err)
	require.NotEqual(t, env.Uuid, created.Uuid)
	rule, err := db.CreateModuleRule(writerCtx, writer, env.OrgId, &model.DefinitionRule{DefinitionId: "module", ResourceType: "generic", ProjectId: opt.Of(env.ProjectId), EnvId: opt.Of(env.Id), CreatedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, writer.Commit())
	require.NoError(t, held.Commit())
	select {
	case response := <-deleted:
		require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("final deletion did not recheck lifecycle")
	}
	current, err := db.GetEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, created.Uuid, current.Uuid)
	rules, _, err := db.ListModuleRules(t.Context(), nil, env.OrgId, "", 20, model.ListModuleRulesParams{})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, rule.Id, rules[0].Id)
	require.Equal(t, before, finalDeletionOutboxCount(t, db))
}
