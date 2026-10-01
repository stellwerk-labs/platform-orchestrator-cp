//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func TestPluginEventsRequireScopedServiceAuthority(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "resource-events.read", "events.publish")
	service, checker := userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	permission := "plugin." + grant.PluginID + ".read"
	item := plugins.Installation{ID: grant.PluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{grant.OrgID: plugins.StateAvailable}, OrganizationTimezones: map[string]string{grant.OrgID: "Europe/Berlin"}, ServiceGrants: []plugins.ServiceGrant{
		{Action: "resource-events.read", Permission: permission, Scope: "environment"}, {Action: "events.publish", Permission: permission, Scope: "environment"},
	}}
	var denied atomic.Bool
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		assert.Equal(t, grant.ActorID, body.UserId)
		assert.Equal(t, []iamapi.ResourcePermissionCheck{environmentCheck(grant.Intent.Environments[0].EnvironmentID, permission)}, body.Checks)
		if denied.Load() {
			w.WriteHeader(403)
		} else {
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(iam.Close)
	client, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	s := &Server{Logger: zap.NewNop(), Database: db, IamClient: client, PluginGateway: &plugins.Gateway{}}
	setInstallation := func() {
		encoded, err := json.Marshal([]plugins.Installation{item})
		require.NoError(t, err)
		s.PluginGateway.Registry, err = plugins.LoadInstallations(bytes.NewReader(encoded), false)
		require.NoError(t, err)
	}
	setInstallation()
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-events-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	path := "/orgs/" + grant.OrgID + "/plugins/" + grant.PluginID + "/operations/" + grant.OperationID.String()
	call := func(actor, method, suffix string, body any) *httptest.ResponseRecorder {
		var encoded []byte
		if body != nil {
			var err error
			encoded, err = json.Marshal(body)
			require.NoError(t, err)
		}
		request := httptest.NewRequest(method, path+suffix, bytes.NewReader(encoded))
		request.Header.Set("From", actor)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	body := PublishPluginEventJSONRequestBody{EventId: uuid.New(), GrantFence: grant.Revision, Type: "plugin." + grant.PluginID + ".completed", OccurredAt: time.Now().UTC(), Data: map[string]any{"status": "Succeeded"}}
	for _, actor := range []string{uuid.NewString(), checker, "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		require.Equal(t, 403, call(actor, http.MethodGet, "/resource-events", nil).Code)
		require.Equal(t, 403, call(actor, http.MethodPost, "/events", body).Code)
	}
	for _, availability := range []plugins.AvailabilityState{plugins.StateAvailable, plugins.StateNotEnabled, plugins.StateNotEntitled} {
		item.Organizations[grant.OrgID] = availability
		setInstallation()
		response := call(service, http.MethodGet, "/resource-events?limit=1", nil)
		require.Equal(t, 200, response.Code, response.Body.String())
		var page ReadPluginResourceEvents200JSONResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
		require.Equal(t, "Europe/Berlin", page.OrganizationTimezone)
		require.NotEmpty(t, page.NextCursor)
		response = call(service, http.MethodGet, "/resource-events?after="+page.NextCursor, nil)
		require.Equal(t, 200, response.Code, response.Body.String())
		response = call(service, http.MethodPost, "/events", body)
		require.Equal(t, 204, response.Code, response.Body.String())
	}
	for _, query := range []string{"?limit=201", "?limit=0", "?after=malformed"} {
		require.Equal(t, 400, call(service, http.MethodGet, "/resource-events"+query, nil).Code)
	}
	foreign := body
	foreign.EventId = uuid.New()
	foreign.Type = "io.platform-orchestrator.environment.deleted"
	require.Equal(t, 400, call(service, http.MethodPost, "/events", foreign).Code)
	changed := body
	changed.Data = map[string]any{"status": "Failed"}
	response := call(service, http.MethodPost, "/events", changed)
	require.Equal(t, 409, response.Code)
	require.Contains(t, response.Body.String(), `"error":"plugin_event_id_conflict"`)
	stale := body
	stale.EventId = uuid.New()
	stale.GrantFence++
	response = call(service, http.MethodPost, "/events", stale)
	require.Equal(t, 409, response.Code)
	require.Contains(t, response.Body.String(), `"error":"plugin_event_stale_fence"`)
	denied.Store(true)
	require.Equal(t, 403, call(service, http.MethodGet, "/resource-events", nil).Code)
	require.Equal(t, 403, call(service, http.MethodPost, "/events", body).Code, "idempotent replay still requires current IAM")
	denied.Store(false)
	for _, availability := range []plugins.AvailabilityState{plugins.StateUnavailable, plugins.StateIncompatible} {
		item.Organizations[grant.OrgID] = availability
		setInstallation()
		require.Equal(t, 409, call(service, http.MethodGet, "/resource-events", nil).Code)
		require.Equal(t, 409, call(service, http.MethodPost, "/events", body).Code)
	}
	item.Organizations[grant.OrgID] = plugins.StateAvailable
	item.ServiceGrants = nil
	item.ServiceUserID = ""
	item.AdmissionCheckerID = ""
	setInstallation()
	require.Equal(t, 403, call(service, http.MethodGet, "/resource-events", nil).Code)
}
