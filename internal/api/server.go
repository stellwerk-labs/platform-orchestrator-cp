package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	strictecho "github.com/oapi-codegen/runtime/strictmiddleware/echo"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hmessaging"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/token"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
)

//go:generate go tool oapi-codegen --config=oapi-codegen.cfg.yaml --exclude-tags not-implemented ../../openapi/spec.yaml

type Server struct {
	Database           model.Databaser
	Logger             *zap.Logger
	Vault              vault.VaultClientInterface
	Publisher          hmessaging.Publisher
	DpClient           orchestratordp.ClientWithResponsesInterface
	IamClient          orchestratoriam.ClientWithResponsesInterface
	Plugins            plugins.Registry
	PluginGateway      *plugins.Gateway
	PluginDataPlane    *plugins.DataPlanePort
	PluginEntitlements PluginEntitlementGuard
	// PluginManager is nil while the plugin host is disabled.
	PluginManager *pluginmanager.Manager
	// PluginJournalRetention is how long released operations still reconcile.
	PluginJournalRetention time.Duration
	// DeploymentAdmissionIdentity is the Data Plane's admission identity for
	// every organization; uuid.Nil refuses every admission callback.
	DeploymentAdmissionIdentity uuid.UUID
}

func (s *Server) MapRoutes(e *echo.Echo) {
	apiHandler := NewStrictHandler(s, []StrictMiddlewareFunc{
		adaptStrictMiddleware(hecho.OperationIdCollectorMiddleware),
		adaptStrictMiddleware(hecho.BuildContextTimeoutMiddlewareWithDuration(time.Second * 30)),
		// don't assert authz for an internal APIs
		adaptStrictMiddleware(middleware.NewAuthZAsserter(regexp.MustCompile("^.*Internal.*$"))),
		adaptStrictMiddleware(hecho.AuthMiddleware(string(UserIdHeaderScopes))),
		adaptStrictMiddleware(token.StrictEncryptionMiddleware()),
	})
	RegisterHandlers(e, apiHandler)
	observationapi.RegisterHandlers(e.Group("", echomiddleware.BodyLimit("8M")), observationapi.NewStrictHandler(s, []observationapi.StrictMiddlewareFunc{
		func(handler observationapi.StrictHandlerFunc, operationID string) observationapi.StrictHandlerFunc {
			return observationapi.StrictHandlerFunc(hecho.OperationIdCollectorMiddleware(strictecho.StrictEchoHandlerFunc(handler), operationID))
		},
		func(handler observationapi.StrictHandlerFunc, operationID string) observationapi.StrictHandlerFunc {
			return observationapi.StrictHandlerFunc(hecho.BuildContextTimeoutMiddlewareWithDuration(30*time.Second)(strictecho.StrictEchoHandlerFunc(handler), operationID))
		},
		func(handler observationapi.StrictHandlerFunc, operationID string) observationapi.StrictHandlerFunc {
			return observationapi.StrictHandlerFunc(hecho.AuthMiddleware(string(observationapi.UserIdHeaderScopes))(strictecho.StrictEchoHandlerFunc(handler), operationID))
		},
	}))

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

func adaptStrictMiddleware(middleware strictecho.StrictEchoMiddlewareFunc) StrictMiddlewareFunc {
	return func(handler StrictHandlerFunc, operationID string) StrictHandlerFunc {
		return StrictHandlerFunc(middleware(strictecho.StrictEchoHandlerFunc(handler), operationID))
	}
}

// StrictServerInterface is the interface that your Server implementation should generate methods for.
// This line should fail if you're missing some methods. If you want to add methods to the specification, without
// implementing them, consider tagging them with the "not-implemented" tag.
var _ StrictServerInterface = (*Server)(nil)

// MustDecodeOpenApiSpec returns the value from decodeSpec via the cached value in rawSpec and panics if there was an error.
func MustDecodeOpenApiSpec() []byte {
	raw, err := rawSpec()
	if err != nil {
		panic(err)
	}
	var main map[string]json.RawMessage
	if err := json.Unmarshal(raw, &main); err != nil {
		panic(err)
	}
	optional, err := observationapi.GetSpec()
	if err != nil {
		panic(err)
	}
	optionalRaw, err := json.Marshal(optional)
	if err != nil {
		panic(err)
	}
	var extension map[string]json.RawMessage
	if err := json.Unmarshal(optionalRaw, &extension); err != nil {
		panic(err)
	}
	for _, section := range []string{"paths", "components"} {
		if len(extension[section]) == 0 {
			continue
		}
		var current, additions map[string]json.RawMessage
		if err := json.Unmarshal(main[section], &current); err != nil {
			panic(err)
		}
		if err := json.Unmarshal(extension[section], &additions); err != nil {
			panic(err)
		}
		for key, value := range additions {
			if section == "components" && current[key] != nil {
				var entries, extra map[string]json.RawMessage
				_ = json.Unmarshal(current[key], &entries)
				_ = json.Unmarshal(value, &extra)
				for name, item := range extra {
					entries[name] = item
				}
				value, _ = json.Marshal(entries)
			}
			current[key] = value
		}
		main[section], err = json.Marshal(current)
		if err != nil {
			panic(err)
		}
	}
	merged, err := json.Marshal(main)
	if err != nil {
		panic(err)
	}
	return merged
}

func (f N403ForbiddenJSONResponse) IsAuthZFailureResponse() bool {
	return true
}

func (f N401UnauthorizedJSONResponse) IsAuthZFailureResponse() bool {
	return true
}
