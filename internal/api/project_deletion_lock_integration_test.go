//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestNonemptyProjectDeletionRefusesBeforeWaitingForChildRules(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	env, err := db.GetEnvironment(t.Context(), nil, grant.OrgID, "project", "development", model.GetModeDefault)
	require.NoError(t, err)
	project, err := db.GetProject(t.Context(), nil, env.OrgId, env.ProjectId, model.GetModeDefault)
	require.NoError(t, err)
	setup, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = setup.Rollback() }()
	rule, err := db.CreateModuleRule(t.Context(), setup, env.OrgId, &model.DefinitionRule{OrgId: env.OrgId, DefinitionId: "module", ResourceType: "generic", ProjectId: opt.Of(env.ProjectId), EnvId: opt.Of(env.Id), CreatedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, setup.Commit())
	held, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer func() { cancel(); _ = held.Rollback() }()
	_, err = db.GetEnvironment(t.Context(), held, env.OrgId, env.ProjectId, env.Id, model.GetModeForUpdate)
	require.NoError(t, err)
	var locked uuid.UUID
	require.NoError(t, held.QueryRowContext(t.Context(), `SELECT id FROM definition_rules WHERE org_id=$1 AND id=$2 FOR UPDATE`, env.OrgId, rule.Id).Scan(&locked))
	require.Equal(t, rule.Id, locked)
	var outboxBefore int
	require.NoError(t, held.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outboxBefore))
	router := finalDeletionRouter(t, db)
	request := httptest.NewRequest(http.MethodDelete, "/orgs/"+env.OrgId+"/projects/"+env.ProjectId+"?deleteRules=true", nil).WithContext(ctx)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.NoError(t, ctx.Err(), "a nonempty Project must refuse before waiting on its children")
	currentProject, err := db.GetProject(t.Context(), nil, env.OrgId, env.ProjectId, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, project, currentProject)
	currentEnv, err := db.GetEnvironment(t.Context(), nil, env.OrgId, env.ProjectId, env.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, env, currentEnv)
	rules, _, err := db.ListModuleRules(t.Context(), nil, env.OrgId, "", 2, model.ListModuleRulesParams{})
	require.NoError(t, err)
	require.Equal(t, []model.DefinitionRule{*rule}, rules)
	var outboxAfter int
	require.NoError(t, held.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outboxAfter))
	require.Equal(t, outboxBefore, outboxAfter)
}
