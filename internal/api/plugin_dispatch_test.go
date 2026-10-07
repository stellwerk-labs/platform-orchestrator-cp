package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPluginDispatchRejectsUnconfiguredServicesBeforeDataAccess(t *testing.T) {
	pluginID := "dev.example.workflow"
	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	item := plugins.Installation{ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateNotEnabled}, ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.admit", Permission: "plugin.dev.example.workflow.control", Scope: "environment"}}}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	server := &Server{Logger: zap.NewNop(), PluginGateway: &plugins.Gateway{Registry: registry}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	body := `{"fence":1,"action":"external-deployment.admit","dispatch_id":"request-1","project_uuid":"` + uuid.NewString() + `","environment_uuid":"` + uuid.NewString() + `","request_digest":"` + strings.Repeat("a", 64) + `"}`
	for _, actor := range []string{service, uuid.NewString(), "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		request := httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/"+pluginID+"/operations/"+uuid.NewString()+"/check", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor)
		request.Header.Set("Po-Org-Id", "acme")
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/"+pluginID+"/operations/"+uuid.NewString()+"/check", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("From", checker)
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	operationPath := "/orgs/acme/plugins/" + pluginID + "/operations/" + uuid.NewString()
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, operationPath, ""},
		{http.MethodPost, "/orgs/acme/plugins/" + pluginID + "/inventory", `{"proof":{"operation_id":"read","idempotency_key":"read-1","assertion":"not-an-assertion"},"selector":{},"module_ids":[]}`},
		{http.MethodPost, "/orgs/acme/plugins/" + pluginID + "/deployment-reads", `{"proof":{"operation_id":"read","idempotency_key":"read-1","assertion":"not-an-assertion"},"kind":"readiness","environments":[{"project_id":"project","env_id":"development"}]}`},
		{http.MethodPost, operationPath + "/authorize", `{"action":"activate","expected_revision":1,"reason":"Start","proof":{"operation_id":"command","idempotency_key":"start-1","assertion":"not-an-assertion"}}`},
		{http.MethodPost, operationPath + "/modules", `{"action":"reserve","grant_fence":1,"expected_fence":0,"request_id":"reserve-1","reason":"Reserve","targets":[{"module_uuid":"` + uuid.NewString() + `","version_uuid":"` + uuid.NewString() + `","expected_resource_version":1}]}`},
		{http.MethodPost, operationPath + "/deployments", `{"grant_fence":1,"request_id":"dispatch-1","environment_uuid":"` + uuid.NewString() + `","mode":"deploy","plan":{"snapshot_id":"` + uuid.NewString() + `","fingerprint":"sha256:` + strings.Repeat("a", 64) + `","module_versions":{"` + uuid.NewString() + `":"` + uuid.NewString() + `"}}}`},
		{http.MethodPost, operationPath + "/deployments/dispatch-1/reconcile", `{"grant_fence":1,"withdraw":true}`},
		{http.MethodPost, operationPath + "/pins", `{"action":"cancel","grant_fence":1,"request_id":"cancel-1","reason":"Cancel unstarted override","targets":[{"pin_id":"` + uuid.NewString() + `","expected_resource_version":1}]}`},
	} {
		t.Run(route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("From", checker)
			request.Header.Set("Po-Org-Id", "acme")
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
}
