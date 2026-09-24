package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetPluginCapabilityDistinguishesUnavailableStates(t *testing.T) {
	e, server, cleanup := MockServer(t)
	defer cleanup()
	server.Plugins = plugins.NewStaticRegistry(plugins.Descriptor{
		ID:           "dev.example.change-orchestrator",
		DisplayName:  "Change Orchestrator",
		Version:      "1.0.0",
		Capabilities: []string{"change.read"},
		State:        plugins.StateNotEntitled,
		AdminAction:  "Assign a Change Orchestrator entitlement to this organization.",
	})

	request := httptest.NewRequest(http.MethodGet, "/orgs/acme/plugins/dev.example.change-orchestrator", nil)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, `{
		"id":"dev.example.change-orchestrator",
		"display_name":"Change Orchestrator",
		"version":"1.0.0",
		"state":"not_entitled",
		"capabilities":["change.read"],
		"admin_action":"Assign a Change Orchestrator entitlement to this organization."
	}`, response.Body.String())
}

func TestGetPluginCapabilityReportsUnknownPluginAsNotInstalled(t *testing.T) {
	e, _, cleanup := MockServer(t)
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/orgs/acme/plugins/dev.stellwerk.missing", nil)
	request.Header.Set("From", userid.InternalSystemUuid.String())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"state":"not_installed"`)
}
