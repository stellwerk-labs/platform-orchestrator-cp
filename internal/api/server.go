package api

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hmessaging"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/token"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
)

//go:generate go tool oapi-codegen --config=oapi-codegen.cfg.yaml --exclude-tags not-implemented ../../openapi/spec.yaml

type Server struct {
	Database  model.Databaser
	Logger    *zap.Logger
	Vault     vault.VaultClientInterface
	Publisher hmessaging.Publisher
	DpClient  orchestratordp.ClientWithResponsesInterface
	IamClient orchestratoriam.ClientWithResponsesInterface
}

func (s *Server) MapRoutes(e *echo.Echo) {
	apiHandler := NewStrictHandler(s, []StrictMiddlewareFunc{
		hecho.OperationIdCollectorMiddleware,
		hecho.BuildContextTimeoutMiddlewareWithDuration(time.Second * 30),
		// don't assert authz for an internal APIs
		middleware.NewAuthZAsserter(regexp.MustCompile("^.*Internal.*$")),
		hecho.AuthMiddleware(UserIdHeaderScopes),
		token.StrictEncryptionMiddleware(),
	})
	RegisterHandlers(e, apiHandler)

	buildInfo, _ := debug.ReadBuildInfo()
	e.GET("/alive", func(c echo.Context) error {
		return c.String(200, fmt.Sprintf("%s %s", buildInfo.Main.Path, buildInfo.Main.Version))
	})
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(200, map[string]string{
			"app":     buildInfo.Main.Path,
			"version": buildInfo.Main.Version,
			"status":  "OK",
		})
	})
}

// StrictServerInterface is the interface that your Server implementation should generate methods for.
// This line should fail if you're missing some methods. If you want to add methods to the specification, without
// implementing them, consider tagging them with the "not-implemented" tag.
var _ StrictServerInterface = (*Server)(nil)

// MustDecodeOpenApiSpec returns the value from decodeSpec via the cached value in rawSpec and panics if there was an error.
func MustDecodeOpenApiSpec() []byte {
	if b, err := rawSpec(); err != nil {
		panic(err)
	} else {
		return b
	}
}

func (f N403ForbiddenJSONResponse) IsAuthZFailureResponse() bool {
	return true
}

func (f N401UnauthorizedJSONResponse) IsAuthZFailureResponse() bool {
	return true
}
