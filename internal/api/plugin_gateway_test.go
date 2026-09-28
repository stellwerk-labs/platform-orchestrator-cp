package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPluginGatewayPublicRoutesDisabledByDefault(t *testing.T) {
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server := Server{Logger: zap.NewNop()}
	server.MapRoutes(e)
	request := httptest.NewRequest(http.MethodGet, "/orgs/acme/plugins", nil)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.JSONEq(t, `{"items":[]}`, response.Body.String())
	request = httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/dev.example.change-manager/invoke", strings.NewReader(`{"operation_id":"list"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response = httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"plugin_not_installed"`)
}

func TestPluginDiscoveryDoesNotExposeBackendAndAvailabilityDeniesForwardCall(t *testing.T) {
	registry, err := plugins.LoadInstallations(strings.NewReader(`[{"id":"dev.example.change-manager","display_name":"Change Manager","version":"1.0.0","backend":"https://internal-plugin.invalid","organizations":{"acme":"not_entitled"},"capabilities":[],"ui":{"sdk_version":"1","origin":"https://plugin.invalid","entrypoint":"/index.html","route_prefix":"/changes","navigation_label":"Changes"},"operations":[{"id":"read","method":"GET","path":"/orgs/{orgId}/changes","mutating":false},{"id":"start","method":"POST","path":"/orgs/{orgId}/changes/start","mutating":true}]}]`), false)
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	server := Server{Logger: zap.NewNop(), Plugins: registry, PluginGateway: &plugins.Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}}
	server.MapRoutes(e)
	request := httptest.NewRequest(http.MethodGet, "/orgs/acme/plugins", nil)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page PluginContributionPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, PluginContributionMode("safety_only"), page.Items[0].Mode)
	require.Len(t, page.Items[0].Operations, 1)
	require.NotContains(t, response.Body.String(), "internal-plugin")
	request = httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/dev.example.change-manager/invoke", strings.NewReader(`{"operation_id":"start","idempotency_key":"once"}`))
	request.Header.Set("From", userid.InternalSystemUuid.String())
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error":"plugin_not_entitled"`)
}
