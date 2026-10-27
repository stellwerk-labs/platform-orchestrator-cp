package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
)

func TestGeneratedOpenAPISchemaValidatesWithExamples(t *testing.T) {
	t.Parallel()
	document, err := GetSwagger()
	require.NoError(t, err)
	require.NoError(t, document.Validate(t.Context(), openapi3.EnableExamplesValidation()))
}

func TestGeneratedRoutesPreserveAuthentication(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, actor string
		status      int
	}{
		{name: "missing identity", status: http.StatusUnauthorized},
		{name: "invalid identity", actor: "not-a-user-id", status: http.StatusUnauthorized},
		{name: "authenticated internal discovery", actor: userid.InternalSystemUuid.String(), status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{
				AppName: "generated-auth-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec(),
			})
			require.NoError(t, err)
			server.Use(middleware.EchoCtxMiddleware)
			(&Server{Logger: zap.NewNop()}).MapRoutes(server)
			request := httptest.NewRequest(http.MethodGet, "/orgs/acme/plugins", nil)
			if test.actor != "" {
				request.Header.Set("From", test.actor)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code, response.Body.String())
		})
	}
}
