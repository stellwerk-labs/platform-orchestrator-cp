package middleware

import (
	"context"
	"regexp"

	"github.com/labstack/echo/v4"
	strictecho "github.com/oapi-codegen/runtime/strictmiddleware/echo"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hlogger"
)

const authChecked = "github.com/stellwerk-labs/platform-orchestrator-iam/auth-checked"

// interface used to match against N401/N403 JSON response types
type authZFailureResponse interface {
	IsAuthZFailureResponse() bool
}

func NewAuthZAsserter(skipOperationPattern *regexp.Regexp) strictecho.StrictEchoMiddlewareFunc {
	return func(f strictecho.StrictEchoHandlerFunc, operationID string) strictecho.StrictEchoHandlerFunc {

		// skip this middleware entirely if auth assert is disabled
		if skipOperationPattern.MatchString(operationID) {
			return f
		}

		return func(c echo.Context, request interface{}) (response interface{}, err error) {
			// If we have a user id header, apply it to the standard ids set.
			ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(c.Request().Context())
			c.SetRequest(c.Request().WithContext(ctx))
			if v, ok := c.Request().Context().Value(hecho.ContextKeyUserID).(string); ok && ids.UserId == "" {
				ids.UserId = v
			}

			r, err := f(c, request)
			if i := c.Get(authChecked); i == nil && err == nil {
				// we're ok with letting through 401's and 403's since these are _checking_ the authz
				if fr, ok := r.(authZFailureResponse); !ok || !fr.IsAuthZFailureResponse() {
					return nil, errors.Errorf("all public API methods must authorize the request: %v", r)
				}
			}
			return r, err
		}
	}
}

func SetAuthChecked(c echo.Context) {
	c.Set(authChecked, true)
}

func SetAuthCheckedCtx(ctx context.Context) {
	if i := GetEchoCtx(ctx); i != nil {
		i.Set(authChecked, true)
	}
}
