//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	publiccp "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNativeAgentDisabledResponsesPreserveTypedAbsence(t *testing.T) {
	db := pluginTestDatabase(t)
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "native-read", CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-read", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	spec, err := GetSwagger()
	require.NoError(t, err)
	responseRouter, err := legacy.NewRouter(spec)
	require.NoError(t, err)
	call := func(method, path string, body any, status int) publiccp.NativeAgentRunnerConfiguration {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		route, params, err := responseRouter.FindRoute(request)
		require.NoError(t, err)
		input := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: request, Route: route, PathParams: params},
			Status:                 response.Code,
			Header:                 response.Header(),
			Options:                &openapi3filter.Options{IncludeResponseStatus: true},
		}
		require.NoError(t, openapi3filter.ValidateResponse(t.Context(), input.SetBodyBytes(response.Body.Bytes())), "the actual HTTP response must match its published schema")
		var typed publiccp.Runner
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &typed))
		configuration, err := typed.RunnerConfiguration.AsNativeAgentRunnerConfiguration()
		require.NoError(t, err)
		return configuration
	}
	base, path := "/orgs/native-read/runners", "/orgs/native-read/runners/pool"
	configuration := nativeAgentTestConfiguration(t)
	created := call(http.MethodPost, base, map[string]any{"id": "pool", "runner_configuration": configuration, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "retained"}}, http.StatusCreated)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "native_execution": nil}}, http.StatusOK)
	disabled := call(http.MethodGet, path, nil, http.StatusOK)
	require.Nil(t, disabled.NativeExecution)
	require.NotNil(t, disabled.Capabilities)
	require.NotNil(t, disabled.SupportedProtocols)
	cleared := call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "capabilities": nil, "supported_protocols": nil}}, http.StatusOK)
	require.Nil(t, cleared.NativeExecution)
	require.Nil(t, cleared.Capabilities)
	require.Nil(t, cleared.SupportedProtocols)
	call(http.MethodGet, path, nil, http.StatusOK)
	call(http.MethodGet, "/internal"+path, nil, http.StatusOK)
	call(http.MethodPatch, path, map[string]any{"description": "still disabled"}, http.StatusOK)
	encoded, err := json.Marshal(cleared)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.NotContains(t, fields, "native_execution")
	require.NotContains(t, fields, "capabilities")
	require.NotContains(t, fields, "supported_protocols")
	require.Equal(t, created.Key, cleared.Key)
	require.Equal(t, created.MaxParallelJobs, cleared.MaxParallelJobs)
	reenabled := call(http.MethodPatch, path, map[string]any{"runner_configuration": configuration}, http.StatusOK)
	require.NotNil(t, reenabled.NativeExecution)
	emptyCapabilities := nativeAgentTestConfiguration(t)
	emptyCapabilities["capabilities"] = []string{}
	emptyCapabilities["native_execution"].(map[string]any)["tools"] = []any{}
	empty := call(http.MethodPost, base, map[string]any{"id": "empty-capabilities", "runner_configuration": emptyCapabilities, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "retained"}}, http.StatusCreated)
	require.NotNil(t, empty.Capabilities, "an explicitly declared empty capability list is valid")
}
