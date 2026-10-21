package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

func TestPluginMaterialUsesIndependentCorePermissionAndNeverServiceIdentity(t *testing.T) {
	const pluginID = "dev.example.workflow"
	actor, service, checker := uuid.New(), userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	project, environment, deployment := uuid.New(), uuid.New(), uuid.New()
	item := plugins.Installation{ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service, AdmissionCheckerID: checker, Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateNotEntitled}, ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.read", Permission: "plugin." + pluginID + ".view", Scope: "environment"}}}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var authority atomic.Int32
	var iamCalls, materialCalls atomic.Int32
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iamCalls.Add(1)
		var body iamapi.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(500)
			return
		}
		assert.Equal(t, actor, body.UserId)
		environmentChecks := []iamapi.ResourcePermissionCheck{environmentCheck(environment, "deployment_read")}
		organizationChecks := []iamapi.ResourcePermissionCheck{orgCheck("acme", "deployment_read")}
		assert.Contains(t, [][]iamapi.ResourcePermissionCheck{environmentChecks, organizationChecks}, body.Checks, "no plugin capability or cached operation grant authorizes material")
		if authority.Load() == 1 && reflect.DeepEqual(body.Checks, environmentChecks) || authority.Load() == 2 && reflect.DeepEqual(body.Checks, organizationChecks) {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(iam.Close)
	iamClient, err := iamapi.NewClientWithResponses(iam.URL)
	require.NoError(t, err)
	const original = "{\n  \"result\": \"non-secret test fixture\"\n}\n"
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		materialCalls.Add(1)
		assert.Equal(t, "/orgs/acme/plugins/"+pluginID+"/projects/"+project.String()+"/envs/"+environment.String()+"/external-deployments/"+deployment.String()+"/outputs", r.URL.Path)
		claims, err := plugincontext.Verify(public, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "cp", Audience: "platform-orchestrator-dp", PluginID: pluginID, OrgID: "acme", OperationID: "external-deployment.outputs.read", Method: http.MethodGet, Path: r.URL.EscapedPath(), IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now())
		if !assert.NoError(t, err) {
			w.WriteHeader(401)
			return
		}
		assert.Equal(t, actor.String(), claims.ActorID)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(original))
	}))
	t.Cleanup(dp.Close)
	s := &Server{Logger: zap.NewNop(), IamClient: iamClient, PluginGateway: &plugins.Gateway{Registry: registry, Issuer: "cp", SigningKey: private}}
	s.PluginDataPlane, err = plugins.NewDataPlanePort(dp.URL, s.PluginGateway, true)
	require.NoError(t, err)
	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "material-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	s.MapRoutes(e)
	path := "/orgs/acme/plugins/" + pluginID + "/projects/" + project.String() + "/envs/" + environment.String() + "/deployments/" + deployment.String() + "/outputs"
	get := func(identity string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("From", identity)
		request.Header.Set("Po-Org-Id", "acme")
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		return response
	}
	for _, identity := range []string{service, checker, "ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		require.Equal(t, 403, get(identity).Code)
	}
	require.Zero(t, iamCalls.Load())
	require.Zero(t, materialCalls.Load())
	require.Equal(t, 403, get(actor.String()).Code)
	require.Zero(t, materialCalls.Load())
	authority.Store(1)
	response := get(actor.String())
	require.Equal(t, 200, response.Code, response.Body.String())
	require.True(t, bytes.Equal([]byte(original), response.Body.Bytes()), "original output bytes, no JSON reserialization")
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.Contains(t, response.Header().Get("Content-Disposition"), deployment.String()+".json")
	require.Equal(t, int32(1), materialCalls.Load(), "retained history is readable without entitlement or live Environment lookup")
	authority.Store(2)
	response = get(actor.String())
	require.Equal(t, 200, response.Code, "independent organization authority retains access after Environment IAM relationships are removed")
	require.Equal(t, int32(2), materialCalls.Load())
}
