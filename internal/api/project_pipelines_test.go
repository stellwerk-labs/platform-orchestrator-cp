package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
	"github.com/stretchr/testify/require"
)

func TestPipelineAuthoringRejectsUndeclaredAuthorityBeforeBinding(t *testing.T) {
	tests := []struct {
		name, path, method, body string
		status                   int
	}{
		{"unknown executable", "/orgs/:orgId/projects/:projectId/pipelines", http.MethodPost, `{"slug":"safe","name":"Safe","definition":{"strategy":"reviewed","steps":[]},"exec":"steal"}`, 400},
		{"unknown nested credential", "/orgs/:orgId/projects/:projectId/pipelines", http.MethodPost, `{"slug":"safe","name":"Safe","definition":{"strategy":"reviewed","steps":[],"cost":{"enabled":true,"api_key":"secret"}}}`, 400},
		{"omitted enable cannot disable policy", "/orgs/:orgId/projects/:projectId/pipelines/policy", http.MethodPut, `{"expected_resource_version":5,"allowed_version_ids":[]}`, 400},
		{"null rollback defaults", "/orgs/:orgId/projects/:projectId/pipelines/policy", http.MethodPut, `{"expected_resource_version":5,"enabled":true,"allowed_version_ids":[],"rollback_defaults":null}`, 400},
		{"trailing JSON", "/orgs/:orgId/projects/:projectId/pipelines", http.MethodPost, `{"slug":"safe","name":"Safe","definition":{"strategy":"reviewed","steps":[]}} {}`, 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			request := httptest.NewRequest(test.method, "/", strings.NewReader(test.body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			context := e.NewContext(request, httptest.NewRecorder())
			context.SetPath(test.path)
			entered := false
			err := strictPipelineJSON(func(echo.Context) error { entered = true; return nil })(context)
			require.False(t, entered)
			var httpError *echo.HTTPError
			require.ErrorAs(t, err, &httpError)
			require.Equal(t, test.status, httpError.Code)
		})
	}
}
func TestPipelineOpenAPIIncludesResolvableProjectRoutes(t *testing.T) {
	raw := MustDecodeOpenApiSpec()
	require.Contains(t, string(raw), `pipeline-choices`)
	require.Contains(t, string(raw), `"PipelineRecipe"`)
	require.Contains(t, string(raw), `"OrgID"`)
}

func TestPipelinePolicyAcceptsBothBoundedEnvironmentMaps(t *testing.T) {
	defaults := map[string]uuid.UUID{}
	version := uuid.New()
	for range 1000 {
		defaults[uuid.NewString()] = version
	}
	body := p.PipelinePolicyWrite{Enabled: true, AllowedVersionIds: []uuid.UUID{version}, DefaultVersionId: &version,
		EnvironmentDefaultVersionIds: defaults, RollbackDefaults: &p.PipelineRollbackDefaults{DefaultVersionId: &version, EnvironmentDefaultVersionIds: defaults}}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 32768)
	request := httptest.NewRequest(http.MethodPut, "/policy", strings.NewReader(string(encoded)))
	require.NoError(t, validatePipelineBody(request, &p.PipelinePolicyWrite{}, []string{"enabled", "allowed_version_ids"}))
	oversized := httptest.NewRequest(http.MethodPut, "/policy", strings.NewReader(strings.Repeat(" ", 256*1024+1)))
	var response *echo.HTTPError
	require.ErrorAs(t, validatePipelineBody(oversized, &p.PipelinePolicyWrite{}, nil), &response)
	require.Equal(t, 413, response.Code)
	source := httptest.NewRequest(http.MethodPost, "/pipelines", strings.NewReader(string(encoded)))
	require.ErrorAs(t, validatePipelineBody(source, &p.PipelineDraftCreate{}, nil), &response)
	require.Equal(t, 413, response.Code)
}
