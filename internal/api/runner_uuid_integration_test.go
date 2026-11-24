//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRunnerUUIDHTTPReadOnlyIdentityAcrossProjections(t *testing.T) {
	db := pluginTestDatabase(t)
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "runner-uuid", CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "runner-uuid", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	call := func(method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		return response
	}
	base := "/orgs/runner-uuid/runners"
	forged := uuid.New()
	body := map[string]any{
		"id": "fixture", "uuid": forged,
		"runner_configuration": map[string]any{
			"type": "serverless-ecs", "auth": map[string]any{"role_arn": "arn:aws:iam::123456789012:role/Runner"},
			"job": map[string]any{"region": "us-east-1", "cluster": "fixture", "subnets": []string{"subnet-fixture"}, "execution_role_arn": "arn:aws:iam::123456789012:role/Execution"},
		},
		"state_storage_configuration": map[string]any{"type": "s3", "bucket": "fixture"},
	}
	call(http.MethodPost, base, body, http.StatusBadRequest)
	delete(body, "uuid")
	var created Runner
	require.NoError(t, json.Unmarshal(call(http.MethodPost, base, body, http.StatusCreated).Body.Bytes(), &created))
	require.NotNil(t, created.Uuid)
	require.NotEqual(t, uuid.Nil, *created.Uuid)
	require.NotEqual(t, forged, *created.Uuid, "rejected caller UUID is not registration authority")
	for _, path := range []string{base + "/fixture", "/internal" + base + "/fixture"} {
		var read Runner
		require.NoError(t, json.Unmarshal(call(http.MethodGet, path, nil, http.StatusOK).Body.Bytes(), &read))
		require.Equal(t, created.Uuid, read.Uuid)
	}
	var page RunnerPage
	require.NoError(t, json.Unmarshal(call(http.MethodGet, base, nil, http.StatusOK).Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, created.Uuid, page.Items[0].Uuid)
	call(http.MethodPatch, base+"/fixture", map[string]any{"uuid": uuid.New(), "description": "rejected"}, http.StatusBadRequest)
	var updated Runner
	require.NoError(t, json.Unmarshal(call(http.MethodPatch, base+"/fixture", map[string]any{"description": "updated"}, http.StatusOK).Body.Bytes(), &updated))
	require.Equal(t, created.Uuid, updated.Uuid)
	require.Equal(t, "updated", *updated.Description)
	stored, err := db.GetRunner(t.Context(), nil, "runner-uuid", "fixture", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, *created.Uuid, stored.Uuid)
	call(http.MethodDelete, base+"/fixture", nil, http.StatusNoContent)
	var recreated Runner
	require.NoError(t, json.Unmarshal(call(http.MethodPost, base, body, http.StatusCreated).Body.Bytes(), &recreated))
	require.NotEqual(t, created.Uuid, recreated.Uuid)
}
