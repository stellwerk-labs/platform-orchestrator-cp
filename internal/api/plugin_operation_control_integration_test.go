//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// PostgreSQL and generated HTTP validation exercise durable, authority-reducing
// commands. The controlled IAM peer verifies the current controller and scope.
func TestPluginOperationServiceControlIsFencedAndAuthorityReducing(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "operation.suspend", "operation.release")
	environment := grant.Intent.Environments[0]
	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".control"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable}, ServiceGrants: []plugins.ServiceGrant{
		{Action: "operation.suspend", Permission: permission, Scope: "environment"},
		{Action: "operation.release", Permission: permission, Scope: "environment"},
	}}
	var denied atomic.Bool
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		assert.Equal(t, grant.ActorID, body.UserId)
		assert.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(environment.EnvironmentID, permission)}, body.Checks)
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(iam.Close)
	client, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: client, PluginGateway: &plugins.Gateway{}}
	item = installPlugin(t, db, item)
	setInstallation := func() {
		s.PluginGateway.Registry, err = plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
		require.NoError(t, err)
	}
	setInstallation()
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "grant-control-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	call := func(actor string, operation uuid.UUID, body any) *httptest.ResponseRecorder {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/orgs/"+grant.OrgID+"/plugins/"+grant.PluginID+"/operations/"+operation.String()+"/control", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("From", actor)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	body := ControlPluginOperationGrantJSONRequestBody{Action: PluginGrantControlSuspend, GrantFence: grant.Revision, RequestId: "suspend-1", Reason: "Pause after execution failure"}
	for _, actor := range []string{uuid.NewString(), checker, "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		response := call(actor, grant.OperationID, body)
		require.Equal(t, 403, response.Code, response.Body.String())
	}
	response := call(service, uuid.New(), body)
	require.Equal(t, 404, response.Code, response.Body.String())
	for _, action := range []string{"activate", "prepare"} {
		invalid := body
		invalid.Action = ControlPluginOperationGrantJSONBodyAction(action)
		response := call(service, grant.OperationID, invalid)
		require.Equal(t, 400, response.Code, response.Body.String())
	}
	response = call(service, grant.OperationID, map[string]any{"action": "release", "grant_fence": grant.Revision, "request_id": "forged-controller", "reason": "Test", "actor_id": uuid.NewString()})
	require.Equal(t, 400, response.Code, response.Body.String())
	for _, state := range []plugins.AvailabilityState{plugins.StateUnavailable, plugins.StateIncompatible} {
		item.Organizations[grant.OrgID] = state
		setInstallation()
		response := call(service, grant.OperationID, body)
		require.Equal(t, 409, response.Code, response.Body.String())
	}
	item.Organizations[grant.OrgID] = plugins.StateNotEnabled
	setInstallation()
	denied.Store(true)
	response = call(service, grant.OperationID, body)
	require.Equal(t, 403, response.Code, response.Body.String())
	denied.Store(false)
	response = call(service, grant.OperationID, body)
	require.Equal(t, 200, response.Code, response.Body.String())
	var suspended PluginOperationGrant
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &suspended))
	require.Equal(t, "suspended", string(suspended.State))
	require.Equal(t, grant.Revision+1, suspended.Revision)
	response = call(service, grant.OperationID, body)
	require.Equal(t, 200, response.Code, response.Body.String())
	denied.Store(true)
	response = call(service, grant.OperationID, body)
	require.Equal(t, 403, response.Code, "replay must still check current IAM")
	denied.Store(false)
	active, err := model.ApplyExternalGrantCommand(t.Context(), db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, SourceOperation: "resume", SourceDigest: strings.Repeat("c", 64), RequestID: "human-resume", ExpectedRevision: suspended.Revision, Action: "activate", Reason: "Resume with a newly authorized user command"})
	require.NoError(t, err)
	response = call(service, grant.OperationID, body)
	require.Equal(t, 200, response.Code, "lost-response replay returns its historical receipt without reducing a later grant")
	current, err := model.GetExternalOperationGrant(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID)
	require.NoError(t, err)
	require.Equal(t, active, current)
	body.RequestId = "late-worker"
	response = call(service, grant.OperationID, body)
	require.Equal(t, 409, response.Code, response.Body.String())
	body.Action, body.GrantFence, body.RequestId = PluginGrantControlRelease, active.Revision, "release-1"
	item.Organizations[grant.OrgID] = plugins.StateNotEntitled
	setInstallation()
	response = call(service, grant.OperationID, body)
	require.Equal(t, 200, response.Code, response.Body.String())
	current, err = model.GetExternalOperationGrant(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID)
	require.NoError(t, err)
	require.Equal(t, "released", current.State)
	require.Equal(t, active.Revision+1, current.Revision)
	item.ServiceGrants = item.ServiceGrants[:1]
	setInstallation()
	response = call(service, grant.OperationID, body)
	require.Equal(t, 403, response.Code, "operator policy revocation also blocks receipt replay")
	item.ServiceGrants = append(item.ServiceGrants, plugins.ServiceGrant{Action: "operation.release", Permission: permission, Scope: "environment"})
	setInstallation()
	unbound := grant.Intent
	unbound.Actions = []string{"external-deployment.read"}
	prepared, err := model.ApplyExternalGrantCommand(t.Context(), db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: uuid.New(), ActorID: grant.ActorID, SourceOperation: "prepare", SourceDigest: strings.Repeat("d", 64), RequestID: "prepare-without-service-control", Action: "prepare", Reason: "No delegated service-control action", Intent: &unbound})
	require.NoError(t, err)
	body.GrantFence, body.RequestId = prepared.Revision, "unbound-release"
	response = call(service, prepared.OperationID, body)
	require.Equal(t, 403, response.Code, "operator policy cannot expand the frozen intent")
}
