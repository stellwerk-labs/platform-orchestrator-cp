package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	api "github.com/stellwerk-labs/platform-orchestrator-cp/internal/pipelineapi"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPipelineDocumentJSONAndYAMLHaveOneContract(t *testing.T) {
	jsonSource := `{"slug":"reviewed","name":"Reviewed","definition":{"strategy":"reviewed","steps":[],"cost":{"enabled":true},"approval":{"mode":"cost_non_increasing"}}}`
	yamlSource := "slug: reviewed\nname: Reviewed\ndefinition:\n  strategy: reviewed\n  steps: []\n  cost:\n    enabled: true\n  approval:\n    mode: cost_non_increasing\n"
	first := validatePipelineDocument([]byte(jsonSource), "application/json")
	second := validatePipelineDocument([]byte(yamlSource), "application/yaml")
	require.True(t, first.Valid, first.Errors)
	require.True(t, second.Valid, second.Errors)
	require.Equal(t, first.Document, second.Document)
}
func TestPipelineDocumentRejectsAmbiguousOrUnsafeSource(t *testing.T) {
	tests := []struct{ name, kind, source string }{
		{"duplicate JSON", "application/json", `{"slug":"first","slug":"second","name":"Name","definition":{"strategy":"apply","steps":[]}}`},
		{"duplicate nested JSON", "application/json", `{"slug":"first","name":"Name","definition":{"strategy":"apply","steps":[],"steps":[]}}`},
		{"YAML through JSON MIME", "application/json", "slug: first\nname: Name\ndefinition: {strategy: apply, steps: []}"},
		{"trailing JSON", "application/json", `{"slug":"first","name":"Name","definition":{"strategy":"apply","steps":[]}} {}`},
		{"duplicate YAML", "application/yaml", "slug: first\nslug: second\nname: Name\ndefinition: {strategy: apply, steps: []}"},
		{"anchor without alias", "application/yaml", "slug: &slug first\nname: Name\ndefinition: {strategy: apply, steps: []}"},
		{"alias", "application/yaml", "slug: &slug first\nname: *slug\ndefinition: {strategy: apply, steps: []}"},
		{"merge key", "application/yaml", "slug: first\nname: Name\ndefinition: {strategy: apply, steps: [], <<: {}}"},
		{"explicit tag", "application/yaml", "slug: !!str first\nname: Name\ndefinition: {strategy: apply, steps: []}"},
		{"nonstring key", "application/yaml", "slug: first\nname: Name\ndefinition: {strategy: apply, steps: [], 7: value}"},
		{"multiple YAML", "application/yaml", "slug: first\nname: Name\ndefinition: {strategy: apply, steps: []}\n---\n{}"},
		{"deep JSON", "application/json", strings.Repeat("[", 26) + "0" + strings.Repeat("]", 26)},
		{"deep YAML", "application/yaml", strings.Repeat("[", 26) + "0" + strings.Repeat("]", 26)},
		{"invalid UTF8", "application/json", string([]byte{0xff})},
		{"oversize", "application/json", strings.Repeat(" ", maxPipelineDocumentBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validatePipelineDocument([]byte(test.source), test.kind)
			require.False(t, result.Valid)
			require.Nil(t, result.Document)
			require.NotEmpty(t, result.Errors)
			require.Equal(t, "source", result.Errors[0].Code)
		})
	}
}
func TestPipelineDocumentSchemaAndSemanticRejection(t *testing.T) {
	tests := []struct{ name, source, code string }{
		{"server-owned ID", `{"id":"secret","slug":"first","name":"Name","definition":{"strategy":"apply","steps":[]}}`, "properties"},
		{"unknown nested value", `{"slug":"first","name":"Name","definition":{"strategy":"reviewed","steps":[],"cost":{"enabled":true,"api_key":"secret"}}}`, "properties"},
		{"null steps", `{"slug":"first","name":"Name","definition":{"strategy":"apply","steps":null}}`, "nullable"},
		{"missing enabled", `{"slug":"first","name":"Name","definition":{"strategy":"reviewed","steps":[],"cost":{}}}`, "required"},
		{"wrong name type", `{"slug":"first","name":7,"definition":{"strategy":"apply","steps":[]}}`, "type"},
		{"invalid tool phase", `{"slug":"first","name":"Name","definition":{"strategy":"reviewed","steps":[{"id":"validate","tool_id":"validate-manifest","phase":"post_apply","parameters":{}}]}}`, "semantics"},
		{"cost without plan", `{"slug":"first","name":"Name","definition":{"strategy":"apply","steps":[],"cost":{"enabled":true}}}`, "semantics"},
		{"conditional without cost", `{"slug":"first","name":"Name","definition":{"strategy":"reviewed","steps":[],"approval":{"mode":"cost_non_increasing"}}}`, "semantics"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validatePipelineDocument([]byte(test.source), "application/json")
			require.False(t, result.Valid)
			require.Nil(t, result.Document)
			require.NotEmpty(t, result.Errors)
			require.Equal(t, test.code, result.Errors[0].Code)
			require.NotContains(t, result.Errors[0].Message, "secret")
		})
	}
	result := validatePipelineDocument([]byte(`{"slug":"first","name":"`+strings.Repeat("ü", 120)+`","definition":{"strategy":"apply","steps":[]}}`), "application/json")
	require.True(t, result.Valid, result.Errors)
}
func TestPipelineDocumentSourceBoundaryNormalizesBeforeOpenAPI(t *testing.T) {
	e := echo.New()
	e.Pre(pipelineSourceBoundary)
	e.POST("/orgs/:org/projects/:project/pipelines", func(c echo.Context) error {
		require.Equal(t, echo.MIMEApplicationJSON, c.Request().Header.Get(echo.HeaderContentType))
		var value map[string]any
		require.NoError(t, json.NewDecoder(c.Request().Body).Decode(&value))
		require.Equal(t, "first", value["slug"])
		return c.NoContent(201)
	})
	request := httptest.NewRequest(http.MethodPost, "/orgs/acme/projects/project/pipelines", strings.NewReader("slug: first\nname: Name\ndefinition: {strategy: apply, steps: []}"))
	request.Header.Set(echo.HeaderContentType, "application/yaml")
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, 201, response.Code, response.Body.String())
}
func TestPipelineDocumentValidateRetainsNormalAuthentication(t *testing.T) {
	server, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "pipeline-document-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	server.Use(middleware.EchoCtxMiddleware)
	(&Server{Logger: zap.NewNop()}).MapRoutes(server)
	for _, body := range []string{"{{{", `{"slug":"first","name":"Name","definition":{"strategy":"apply","steps":[]}}`} {
		request := httptest.NewRequest(http.MethodPost, "/orgs/acme/projects/project/pipelines/validate", strings.NewReader(body))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		require.Equal(t, 401, response.Code, response.Body.String())
	}
}
func TestPipelineDocumentETagRejectsWildcardAndMultipleVersions(t *testing.T) {
	for _, tag := range []string{"", "3", "*", `W/"3"`, `"3", "4"`, `"0"`, `"-1"`, `"03"`, `"+3"`, `"9223372036854775808"`} {
		_, err := pipelineExpectedVersion(tag)
		require.Error(t, err, tag)
	}
	version, err := pipelineExpectedVersion(`"3"`)
	require.NoError(t, err)
	require.EqualValues(t, 3, version)
}
func TestPipelineDocumentGeneratedSchemaMatchesOpenAPI(t *testing.T) {
	// Resolve the generated draft-07 projection through kin-openapi after restoring
	// local refs. This catches drift between server validation and editor schemas.
	var document map[string]any
	require.NoError(t, json.Unmarshal(pipelineDocumentSchema, &document))
	require.Equal(t, "http://json-schema.org/draft-07/schema#", document["$schema"])
	definitions := document["definitions"].(map[string]any)
	delete(document, "definitions")
	delete(document, "$schema")
	delete(document, "title")
	raw, err := json.Marshal(map[string]any{"openapi": "3.0.3", "info": map[string]any{"title": "projection", "version": "1"}, "paths": map[string]any{}, "components": map[string]any{"schemas": definitions}})
	require.NoError(t, err)
	raw = []byte(strings.ReplaceAll(string(raw), "#/definitions/", "#/components/schemas/"))
	projected, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err)
	source, err := pipelineSchema()
	require.NoError(t, err)
	originalRaw, err := json.Marshal(source)
	require.NoError(t, err)
	// The root schema itself is compared after projecting references back.
	projectedRoot, err := json.Marshal(document)
	require.NoError(t, err)
	projectedRoot = []byte(strings.ReplaceAll(string(projectedRoot), "#/definitions/", "#/components/schemas/"))
	require.JSONEq(t, string(originalRaw), string(projectedRoot))
	require.NotEmpty(t, projected.Components.Schemas)
	original, err := pipelineSchema()
	require.NoError(t, err)
	require.NotNil(t, original)
	spec, err := api.GetSpec()
	require.NoError(t, err)
	for name, projectedDefinition := range projected.Components.Schemas {
		expected, err := json.Marshal(spec.Components.Schemas[name])
		require.NoError(t, err)
		actual, err := json.Marshal(projectedDefinition)
		require.NoError(t, err)
		require.JSONEq(t, string(expected), string(actual), name)
	}
}
