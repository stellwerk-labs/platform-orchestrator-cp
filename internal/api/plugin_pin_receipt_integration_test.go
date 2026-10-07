//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// Real HTTP/generated contracts and PostgreSQL exercise response-loss recovery.
// IAM is a controlled HTTP peer to verify exact current actor/scope decisions.
func TestPluginPinReceiptRecoveryRequiresCurrentScopedAuthority(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "pin.override.begin")
	env, module := grant.Intent.Environments[0], grant.Intent.Modules[0]
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	pin, err := db.CreateEnvironmentModuleVersionPin(t.Context(), tx, grant.OrgID, env.ProjectID, "project", env.EnvironmentID, "development", module, *grant.BaselineDefaults[module.String()], grant.ActorID, "user", "Protect the baseline", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	command := model.ExternalPinCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, GrantFence: grant.Revision, ActorID: grant.ActorID, RequestID: "lost-begin-response", Action: "begin", Reason: "Explicit override", Targets: []model.ExternalPinTarget{{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, TargetVersionUUID: grant.Intent.TargetVersions[module.String()]}}}
	pins, err := model.ApplyExternalPinCommand(t.Context(), db, command, nil)
	require.NoError(t, err)
	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".override"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateNotEntitled}, ServiceGrants: []plugins.ServiceGrant{{Action: "pin.override.begin", Permission: permission, Scope: "environment"}}}
	registry, _ := installedRegistry(t, db, item)
	var denied atomic.Bool
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		assert.Equal(t, grant.ActorID, body.UserId)
		assert.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(env.EnvironmentID, permission)}, body.Checks)
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(iam.Close)
	client, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: client, PluginGateway: &plugins.Gateway{Registry: registry}}
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "pin-recovery-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	call := func(actor, requestID string, fence int64) *httptest.ResponseRecorder {
		path := "/orgs/" + grant.OrgID + "/plugins/" + grant.PluginID + "/operations/" + grant.OperationID.String() + "/pins/receipts/" + requestID + "?grant_fence=" + strconv.FormatInt(fence, 10)
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	for _, actor := range []string{uuid.NewString(), checker, "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		response := call(actor, command.RequestID, grant.Revision)
		require.Equal(t, 403, response.Code, response.Body.String())
	}
	denied.Store(true)
	response := call(service, command.RequestID, grant.Revision)
	require.Equal(t, 403, response.Code, response.Body.String())
	response = call(service, "missing", grant.Revision)
	require.Equal(t, 403, response.Code, "receipt absence is not disclosed to a revoked controller")
	denied.Store(false)
	response = call(service, command.RequestID, grant.Revision+1)
	require.Equal(t, 409, response.Code, response.Body.String())
	response = call(service, "missing", grant.Revision)
	require.Equal(t, 404, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "plugin_pin_receipt_not_found")
	response = call(service, command.RequestID, grant.Revision)
	require.Equal(t, 200, response.Code, response.Body.String())
	var result PluginPinOperationReceipt
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, grant.OperationID, result.OperationId)
	require.Equal(t, command.RequestID, result.RequestId)
	require.Equal(t, "begin", string(result.Action))
	require.Equal(t, []EnvironmentModuleVersionPin{modulePinToAPI(pins[0])}, result.Pins)
	live, err := db.GetEnvironmentModuleVersionPin(t.Context(), nil, grant.OrgID, pin.ID, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, pins[0].ResourceVersion, live.ResourceVersion)
}
