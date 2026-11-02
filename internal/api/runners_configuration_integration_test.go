//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRunnerHTTPUpdatesPreserveLegacyOmissionAndExplicitlyClearNull(t *testing.T) {
	db := pluginTestDatabase(t)
	now := time.Now().UTC()
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "runner-org", CreatedBy: userid.InternalSystemUuid, CreatedAt: now, Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	legacyBody := map[string]any{"type": "serverless-ecs", "auth": map[string]any{"role_arn": "arn:aws:iam::123456789012:role/Runner"}, "job": map[string]any{"region": "us-east-1", "cluster": "fixture", "subnets": []string{"subnet-fixture"}, "execution_role_arn": "arn:aws:iam::123456789012:role/Execution"}}
	encoded, err := json.Marshal(legacyBody)
	require.NoError(t, err)
	configuration := map[string]any{}
	require.NoError(t, json.Unmarshal(encoded, &configuration))
	configuration["max_parallel_jobs"] = 2
	configuration["supported_protocols"] = []string{"environment-observation/v1"}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateRunner(t.Context(), tx, &model.Runner{OrgId: "runner-org", Id: "fixture", RunnerType: "serverless-ecs", RunnerConfiguration: configuration, StateStorageType: "s3", StateStorageConfiguration: map[string]any{"type": "s3", "bucket": "fixture"}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "runner-update-compatibility", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		if body == nil {
			request = httptest.NewRequest(method, path, nil)
		}
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	response := call(http.MethodPatch, "/orgs/runner-org/runners/fixture", map[string]any{"runner_configuration": legacyBody})
	require.Equal(t, 200, response.Code, response.Body.String())
	persisted, err := db.GetRunner(t.Context(), nil, "runner-org", "fixture")
	require.NoError(t, err)
	require.InDelta(t, 2, persisted.RunnerConfiguration["max_parallel_jobs"], 0)
	require.Equal(t, []any{"environment-observation/v1"}, persisted.RunnerConfiguration["supported_protocols"])
	response = call(http.MethodGet, "/orgs/runner-org/runners", nil)
	require.Equal(t, 200, response.Code, response.Body.String())
	var page struct {
		Items []RunnerSummary `json:"items"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, 2, *page.Items[0].RunnerConfiguration.MaxParallelJobs)
	require.Equal(t, []string{"environment-observation/v1"}, *page.Items[0].RunnerConfiguration.SupportedProtocols)
	response = call(http.MethodPatch, "/orgs/runner-org/runners/fixture", map[string]any{"runner_configuration": map[string]any{"type": "serverless-ecs", "max_parallel_jobs": nil}})
	require.Equal(t, 200, response.Code, response.Body.String())
	persisted, err = db.GetRunner(t.Context(), nil, "runner-org", "fixture")
	require.NoError(t, err)
	require.NotContains(t, persisted.RunnerConfiguration, "max_parallel_jobs")
	require.Contains(t, persisted.RunnerConfiguration, "supported_protocols")
	response = call(http.MethodPatch, "/orgs/runner-org/runners/fixture", map[string]any{"runner_configuration": map[string]any{"type": "serverless-ecs", "supported_protocols": nil}})
	require.Equal(t, 200, response.Code, response.Body.String())
	persisted, err = db.GetRunner(t.Context(), nil, "runner-org", "fixture")
	require.NoError(t, err)
	require.NotContains(t, persisted.RunnerConfiguration, "max_parallel_jobs")
	require.NotContains(t, persisted.RunnerConfiguration, "supported_protocols")
	response = call(http.MethodPatch, "/orgs/runner-org/runners/fixture", map[string]any{"runner_configuration": map[string]any{"type": "serverless-ecs", "max_parallel_jobs": 0}})
	require.Equal(t, 400, response.Code, response.Body.String())
}
