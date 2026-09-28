package api

import (
	"crypto/ed25519"
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

func TestPluginDeploymentReadRejectsUndelegatedInspection(t *testing.T) {
	const pluginID = "dev.example.workflow"
	service := uuid.NewString()
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	item := plugins.Installation{ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", ServiceUserID: service, AdmissionCheckerID: uuid.NewString(), Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable},
		ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.readiness", Permission: "plugin." + pluginID + ".view", Scope: "environment"}},
		Operations:    []plugins.Operation{{ID: "preview", Method: "POST", Path: "/orgs/{orgId}/preview", ServiceActions: []string{"external-deployment.readiness"}}},
	}
	encoded, err := json.Marshal([]plugins.Installation{item})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)
	server := &Server{Logger: zap.NewNop(), PluginGateway: &plugins.Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(e)
	proof := `"proof":{"operation_id":"preview","idempotency_key":"preview-1","assertion":"not-a-valid-proof"}`
	for _, test := range []struct {
		name, fields string
		status       int
	}{
		{"forged-proof", `"kind":"readiness","environments":[{"project_id":"project","env_id":"development"}]`, 403},
		{"undeclared-service", `"kind":"deployment","environments":[{"project_id":"project","env_id":"development"}],"deployment_id":"` + uuid.NewString() + `"`, 403},
		{"mixed-readiness-targets", `"kind":"readiness","environments":[{"project_id":"project","env_id":"development"}],"module_versions":{"module":"` + uuid.NewString() + `"}`, 400},
		{"plan-without-targets", `"kind":"plan","environments":[{"project_id":"project","env_id":"development"}]`, 400},
		{"deployment-without-identity", `"kind":"deployment","environments":[{"project_id":"project","env_id":"development"}]`, 400},
		{"unknown-action", `"kind":"delete","environments":[{"project_id":"project","env_id":"development"}]`, 400},
		{"unbounded-empty", `"kind":"readiness","environments":[]`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/"+pluginID+"/deployment-reads", strings.NewReader(`{`+proof+`,`+test.fields+`}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("From", service)
			response := httptest.NewRecorder()
			e.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code, response.Body.String())
		})
	}
}
