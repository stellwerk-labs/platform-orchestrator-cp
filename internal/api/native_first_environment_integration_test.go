//go:build integration

package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func nativeFirstRouter(t *testing.T, db model.Databaser, publisher *hmessaging.RecordingPublisher) *echo.Echo {
	t.Helper()
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-first", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop(), Publisher: publisher}).MapRoutes(router)
	return router
}

func nativeFirstRequest(router *echo.Echo, method, path string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("From", userid.InternalSystemUuid.String())
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func nativeFirstRules(t *testing.T, db model.Databaser, org string, rules ...model.RunnerRule) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `DELETE FROM runner_rules WHERE org_id=$1`, org)
	require.NoError(t, err)
	for _, r := range rules {
		// Stored ties also occur after concurrent old writers both checked an absent rule.
		_, err = tx.ExecContext(t.Context(), `INSERT INTO runner_rules(org_id,runner_id,project_id,env_type_id,created_at) VALUES($1,$2,$3,$4,now())`, org, r.RunnerId, r.ProjectId.Ref(), r.EnvTypeId.Ref())
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

func nativeFirstExtraRunner(t *testing.T, db model.Databaser, org, id string, native bool) *model.Runner {
	t.Helper()
	config := nativeAgentTestConfiguration(t)
	if !native {
		config = map[string]any{"type": "kubernetes-agent", "key": config["key"], "job": map[string]any{"namespace": "runner", "service_account": "runner"}}
	}
	raw, err := json.Marshal(map[string]any{"id": id, "runner_configuration": config, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "owned-states"}})
	require.NoError(t, err)
	response := nativeFirstRequest(finalDeletionRouter(t, db), http.MethodPost, "/orgs/"+org+"/runners", raw)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	runner, err := db.GetRunner(t.Context(), nil, org, id, model.GetModeDefault)
	require.NoError(t, err)
	return runner
}

func TestNativeFirstEnvironmentCreationPolicy(t *testing.T) {
	db, first, admission := nativeTargetAdmissionFixture(t)
	second := nativeFirstExtraRunner(t, db, admission.OrgID, "other_native", true)
	legacy := nativeFirstExtraRunner(t, db, admission.OrgID, "legacy_pool", false)
	publisher := &hmessaging.RecordingPublisher{}
	router := nativeFirstRouter(t, db, publisher)
	for _, trial := range []struct {
		name     string
		rules    []model.RunnerRule
		want     int
		eligible bool
	}{
		{"no-rule", nil, 201, false},
		{"legacy", []model.RunnerRule{{RunnerId: legacy.Id}}, 201, false},
		{"native", []model.RunnerRule{{RunnerId: first.Id}}, 201, true},
		{"native-specific", []model.RunnerRule{{RunnerId: legacy.Id}, {RunnerId: first.Id, ProjectId: opt.Of("project")}}, 201, true},
		{"legacy-specific", []model.RunnerRule{{RunnerId: first.Id}, {RunnerId: legacy.Id, ProjectId: opt.Of("project")}}, 201, false},
		{"same-native-tie", []model.RunnerRule{{RunnerId: first.Id}, {RunnerId: first.Id}}, 201, true},
		{"native-tie", []model.RunnerRule{{RunnerId: first.Id}, {RunnerId: second.Id}}, 409, false},
		{"mixed-tie", []model.RunnerRule{{RunnerId: first.Id}, {RunnerId: legacy.Id}}, 409, false},
	} {
		t.Run(trial.name, func(t *testing.T) {
			nativeFirstRules(t, db, admission.OrgID, trial.rules...)
			before := len(publisher.Messages())
			response := nativeFirstRequest(router, http.MethodPost, "/orgs/"+admission.OrgID+"/projects/project/envs", []byte(`{"id":"`+trial.name+`","env_type_id":"development","labels":{"native_first_eligible":"true"}}`))
			require.Equal(t, trial.want, response.Code, response.Body.String())
			env, err := db.GetEnvironment(t.Context(), nil, admission.OrgID, "project", trial.name, model.GetModeDefault)
			if trial.want != 201 {
				_, missing := model.IsErrNotFound(err)
				require.True(t, missing)
				require.Len(t, publisher.Messages(), before)
				return
			}
			require.NoError(t, err)
			require.Equal(t, trial.eligible, env.NativeFirstEligible)
			require.Equal(t, trial.eligible, env.RunnerId.IsSet())
			if trial.eligible {
				require.Equal(t, first.Id, env.RunnerId.Must())
			}
			var body Environment
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.NotNil(t, body.NativeFirstEligible)
			require.Equal(t, env.NativeFirstEligible, *body.NativeFirstEligible)
			require.Len(t, publisher.Messages(), before+1)
			byUUID, err := db.GetEnvironmentByUuid(t.Context(), nil, env.OrgId, env.Uuid, model.GetModeDefault)
			require.NoError(t, err)
			require.Equal(t, env.NativeFirstEligible, byUUID.NativeFirstEligible)
		})
	}
	forged := nativeFirstRequest(router, http.MethodPost, "/orgs/"+admission.OrgID+"/projects/project/envs", []byte(`{"id":"forged","env_type_id":"development","native_first_eligible":true}`))
	require.Equal(t, http.StatusBadRequest, forged.Code, forged.Body.String())
	_, forgedErr := db.GetEnvironment(t.Context(), nil, admission.OrgID, "project", "forged", model.GetModeDefault)
	_, missing := model.IsErrNotFound(forgedErr)
	require.True(t, missing)
	nativeFirstRules(t, db, admission.OrgID, model.RunnerRule{RunnerId: first.Id})
	config := first.RunnerConfiguration
	delete(config, "native_execution")
	_, err := db.UpdateRunner(t.Context(), nil, first.OrgId, first.Id, &model.RunnerPatch{RunnerConfiguration: &config, UpdatedAt: time.Now()})
	require.NoError(t, err)
	before := len(publisher.Messages())
	response := nativeFirstRequest(router, http.MethodPost, "/orgs/"+admission.OrgID+"/projects/project/envs", []byte(`{"id":"disabled","env_type_id":"development"}`))
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Len(t, publisher.Messages(), before)
}

func TestNativeFirstEligibilityMigrationKeepsHistoricalAndOldWritersUnqualified(t *testing.T) {
	db, runner, a := nativeTargetAdmissionFixture(t)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var schema string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	require.NoError(t, tx.Rollback())
	// Existing migration helpers accept sql.DB; use this owned fixture's original DSN.
	raw := nativeFirstSQLDatabase(t, schema)
	require.NoError(t, goose.DownToContext(t.Context(), raw, "migrations", 63))
	const oldInsert = `INSERT INTO envs(id,org_id,org_uuid,project_id,project_uuid,env_type_id,env_type_uuid,created_at,updated_at,display_name,runner_id,status,labels,provider_state_required)
 SELECT $1,org_id,org_uuid,project_id,project_uuid,env_type_id,env_type_uuid,created_at,updated_at,display_name,runner_id,status,labels,false FROM envs WHERE id='development' AND org_id=$2`
	_, err = raw.ExecContext(t.Context(), oldInsert, "old-before", a.OrgID)
	require.NoError(t, err)
	require.NoError(t, goose.UpContext(t.Context(), raw, "migrations"))
	_, err = raw.ExecContext(t.Context(), oldInsert, "old-after", a.OrgID)
	require.NoError(t, err)
	for _, id := range []string{"development", "old-before", "old-after"} {
		env, err := db.GetEnvironment(t.Context(), nil, a.OrgID, "project", id, model.GetModeDefault)
		require.NoError(t, err)
		require.False(t, env.NativeFirstEligible)
	}
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: runner.Id})
	response := nativeFirstRequest(nativeFirstRouter(t, db, &hmessaging.RecordingPublisher{}), http.MethodPost, "/orgs/"+a.OrgID+"/projects/project/envs", []byte(`{"id":"eligible","env_type_id":"development"}`))
	require.Equal(t, 201, response.Code, response.Body.String())
	require.ErrorContains(t, goose.DownToContext(t.Context(), raw, "migrations", 63), "cannot discard Native-first")
	env, err := db.GetEnvironment(t.Context(), nil, a.OrgID, "project", "eligible", model.GetModeDefault)
	require.NoError(t, err)
	require.True(t, env.NativeFirstEligible)
	projectRows, _, err := db.ListEnvironments(t.Context(), nil, a.OrgID, "project", "", 100, model.ListEnvironmentsParams{})
	require.NoError(t, err)
	orgRows, _, err := db.ListEnvironmentsInOrg(t.Context(), nil, a.OrgID, "", 100, model.ListEnvironmentsParams{})
	require.NoError(t, err)
	uuidRows, _, err := db.ListEnvironmentsByProjectUuid(t.Context(), nil, a.OrgID, a.ProjectUUID, "", 100)
	require.NoError(t, err)
	runnerRows, _, err := db.ListEnvironmentsByRunnerId(t.Context(), nil, a.OrgID, runner.Id, "", 100)
	require.NoError(t, err)
	for _, rows := range [][]model.Environment{projectRows, orgRows, uuidRows, runnerRows} {
		var found bool
		for _, row := range rows {
			require.Equal(t, row.Uuid == env.Uuid, row.NativeFirstEligible)
			found = found || row.Uuid == env.Uuid
		}
		require.True(t, found)
	}
	router := nativeFirstRouter(t, db, &hmessaging.RecordingPublisher{})
	path := "/orgs/" + a.OrgID + "/projects/project/envs/eligible"
	patch := nativeFirstRequest(router, http.MethodPatch, path, []byte(`{"native_first_eligible":false}`))
	require.Equal(t, http.StatusBadRequest, patch.Code, patch.Body.String())
	patch = nativeFirstRequest(router, http.MethodPatch, path, []byte(`{"display_name":"Retained eligibility","labels":{"native_first_eligible":"false"}}`))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	var updated Environment
	require.NoError(t, json.Unmarshal(patch.Body.Bytes(), &updated))
	require.NotNil(t, updated.NativeFirstEligible)
	require.True(t, *updated.NativeFirstEligible)
	read := nativeFirstRequest(router, http.MethodGet, "/internal/orgs/"+a.OrgID+"/envs/"+env.Uuid.String(), nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	require.NoError(t, json.Unmarshal(read.Body.Bytes(), &updated))
	require.True(t, *updated.NativeFirstEligible)
}

func TestNativeFirstAssignmentUsesRetainedCustodyAndKeepsUnusedFlexibility(t *testing.T) {
	db, first, a := nativeTargetAdmissionFixture(t)
	second := nativeFirstExtraRunner(t, db, a.OrgID, "other_native", true)
	legacy := nativeFirstExtraRunner(t, db, a.OrgID, "legacy_pool", false)
	publisher := &hmessaging.RecordingPublisher{}
	router := nativeFirstRouter(t, db, publisher)
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: first.Id})
	path := "/orgs/" + a.OrgID + "/projects/project/envs"
	response := nativeFirstRequest(router, http.MethodPost, path, []byte(`{"id":"eligible","env_type_id":"development"}`))
	require.Equal(t, 201, response.Code, response.Body.String())
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: second.Id})
	refresh := func(want int) {
		t.Helper()
		r := nativeFirstRequest(router, http.MethodPost, path+"/eligible/actions/refresh_runner", nil)
		require.Equal(t, want, r.Code, r.Body.String())
	}
	refresh(200)
	state := map[string]any{"type": "s3", "bucket": "before-custody"}
	_, err := db.UpdateRunner(t.Context(), nil, a.OrgID, second.Id, &model.RunnerPatch{StateStorageConfiguration: &state, UpdatedAt: time.Now()})
	require.NoError(t, err)
	refresh(200)
	env, err := db.GetEnvironment(t.Context(), nil, a.OrgID, "project", "eligible", model.GetModeDefault)
	require.NoError(t, err)
	a.AdmissionID = uuid.New()
	a.EnvironmentUUID = env.Uuid
	a.NativeTarget.RunnerID = second.Id
	a.NativeTarget.RunnerUUID = second.Uuid
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	kind, digest, err := genclient.StateStorageConfigurationDigest(raw)
	require.NoError(t, err)
	a.NativeTarget.StateBackendType = string(kind)
	a.NativeTarget.StateConfigurationDigest = digest
	_, err = model.AuthorizeDeploymentAdmission(t.Context(), db, a)
	require.NoError(t, err)
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: first.Id})
	refresh(409)
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: legacy.Id})
	refresh(409)
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: second.Id})
	changed := map[string]any{"type": "s3", "bucket": "after-custody"}
	_, err = db.UpdateRunner(t.Context(), nil, a.OrgID, second.Id, &model.RunnerPatch{StateStorageConfiguration: &changed, UpdatedAt: time.Now()})
	require.NoError(t, err)
	refresh(409)
	_, err = model.SettleDeploymentAdmission(t.Context(), db, a.OrgID, a.AdmissionID, a.ProjectUUID, a.EnvironmentUUID, a.RequestDigest, "failed")
	require.NoError(t, err)
	refresh(409)
	_, err = db.UpdateRunner(t.Context(), nil, a.OrgID, second.Id, &model.RunnerPatch{StateStorageConfiguration: &state, UpdatedAt: time.Now()})
	require.NoError(t, err)
	refresh(200)
	configuration := nativeAgentTestConfiguration(t)
	configuration["max_parallel_jobs"] = 4
	_, err = db.UpdateRunner(t.Context(), nil, a.OrgID, second.Id, &model.RunnerPatch{RunnerConfiguration: &configuration, UpdatedAt: time.Now()})
	require.NoError(t, err)
	refresh(200)
	reread, err := db.GetEnvironment(t.Context(), nil, a.OrgID, "project", "eligible", model.GetModeDefault)
	require.NoError(t, err)
	require.True(t, reread.NativeFirstEligible)
	require.Equal(t, second.Id, reread.RunnerId.Must())
}

func TestNativeFirstAbsenceDoesNotClaimCustody(t *testing.T) {
	db, first, admission := nativeTargetAdmissionFixture(t)
	second := nativeFirstExtraRunner(t, db, admission.OrgID, "other_native", true)
	nativeFirstRules(t, db, admission.OrgID, model.RunnerRule{RunnerId: first.Id})
	router := nativeFirstRouter(t, db, &hmessaging.RecordingPublisher{})
	path := "/orgs/" + admission.OrgID + "/projects/project/envs"
	created := nativeFirstRequest(router, http.MethodPost, path, []byte(`{"id":"unused","env_type_id":"development"}`))
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	env, err := db.GetEnvironment(t.Context(), nil, admission.OrgID, "project", "unused", model.GetModeDefault)
	require.NoError(t, err)
	admission.EnvironmentUUID = env.Uuid
	sealed, err := model.SealDeploymentAdmissionAbsence(t.Context(), db, admission)
	require.NoError(t, err)
	require.Equal(t, "absent", sealed.Outcome)
	nativeFirstRules(t, db, admission.OrgID, model.RunnerRule{RunnerId: second.Id})
	refreshed := nativeFirstRequest(router, http.MethodPost, path+"/unused/actions/refresh_runner", nil)
	require.Equal(t, http.StatusOK, refreshed.Code, refreshed.Body.String())
	env, err = db.GetEnvironment(t.Context(), nil, admission.OrgID, "project", "unused", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, second.Id, env.RunnerId.Must())
	require.True(t, env.NativeFirstEligible)
}

func TestNativeFirstCreationRechecksDisabledRunnerAfterItsLock(t *testing.T) {
	db, runner, a := nativeTargetAdmissionFixture(t)
	nativeFirstRules(t, db, a.OrgID, model.RunnerRule{RunnerId: runner.Id})
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var holder int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&holder))
	_, err = db.GetRunner(t.Context(), tx, a.OrgID, runner.Id, model.GetModeForUpdate)
	require.NoError(t, err)
	publisher := &hmessaging.RecordingPublisher{}
	router := nativeFirstRouter(t, db, publisher)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- nativeFirstRequest(router, http.MethodPost, "/orgs/"+a.OrgID+"/projects/project/envs", []byte(`{"id":"blocked","env_type_id":"development"}`))
	}()
	require.Eventually(t, func() bool {
		check, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return false
		}
		defer func() { _ = check.Rollback() }()
		var waiting bool
		err = check.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	config := runner.RunnerConfiguration
	delete(config, "native_execution")
	_, err = db.UpdateRunner(t.Context(), tx, a.OrgID, runner.Id, &model.RunnerPatch{RunnerConfiguration: &config, UpdatedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	select {
	case response := <-done:
		require.Equal(t, 409, response.Code, response.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("Native creation did not finish after Runner unlock")
	}
	_, err = db.GetEnvironment(t.Context(), nil, a.OrgID, "project", "blocked", model.GetModeDefault)
	_, missing := model.IsErrNotFound(err)
	require.True(t, missing)
	require.Empty(t, publisher.Messages())
}

func nativeFirstSQLDatabase(t *testing.T, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", pgtest.WithSearchPath(os.Getenv("PLUGIN_TEST_DATABASE_URL"), schema))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}
