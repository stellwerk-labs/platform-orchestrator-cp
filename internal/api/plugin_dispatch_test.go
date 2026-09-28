package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPluginDispatchRejectsUnconfiguredServicesBeforeDataAccess(t *testing.T) {
	pluginID := "dev.example.workflow"
	service, checker := uuid.NewString(), uuid.NewString()
	item := plugins.Installation{ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateNotEnabled}, ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.admit", Permission: "plugin.dev.example.workflow.control", Scope: "environment"}}}
	encoded, err := json.Marshal([]plugins.Installation{item})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(strings.NewReader(string(encoded)), false)
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
}
