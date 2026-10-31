package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
)

func TestPluginAuthorizationFreshProofUsesClaimActorAndClosedPolicy(t *testing.T) {
	const pluginID = "dev.example.workflow"
	const org = "acme"
	permission := "plugin." + pluginID + ".rollout-read"
	service, actor := userid.NewServiceUserTokenId().String(), uuid.New()
	item := plugins.Installation{
		ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test",
		Organizations: map[string]plugins.AvailabilityState{org: plugins.StateAvailable}, ServiceUserID: service, AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		ServiceGrants:         []plugins.ServiceGrant{{Action: pluginAuthorizationCheckAction, Permission: permission, Scope: pluginOrganizationScope}},
		AuthorizationPolicies: []plugins.AuthorizationPolicy{{Permission: permission, Scopes: []string{pluginOrganizationScope}}},
		Operations:            []plugins.Operation{{ID: "read", Method: http.MethodGet, Path: "/orgs/{orgId}/workflows", ServiceActions: []string{pluginAuthorizationCheckAction}}},
	}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	_, signingKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	proof := PluginDelegationProof{OperationId: "read", IdempotencyKey: "request-1"}
	proof.Assertion, err = plugincontext.Sign(signingKey, plugincontext.Claims{
		Version: 1, Issuer: "cp", Audience: "plugin:" + pluginID, PluginID: pluginID, OrgID: org, ActorID: actor.String(),
		OperationID: "read", Method: http.MethodGet, Path: "/orgs/acme/workflows", BodySHA256: plugincontext.Digest(nil), RequestID: proof.IdempotencyKey,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}, now)
	require.NoError(t, err)

	ctrl := gomock.NewController(t)
	iam := mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl)
	iam.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: actor,
		Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck(org, permission)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{HTTPResponse: &http.Response{StatusCode: http.StatusNoContent}}, nil)
	server := Server{IamClient: iam, PluginGateway: &plugins.Gateway{Registry: registry, SigningKey: signingKey, Issuer: "cp", Now: func() time.Time { return now }}}
	body := decodePluginAuthorizationBody(t, map[string]any{
		"permission": permission,
		"resources":  []map[string]string{{"type": pluginOrganizationScope, "id": org}},
		"proof":      proof,
	})
	response, err := server.CheckPluginAuthorization(authenticatedPluginContext(t, service), CheckPluginAuthorizationRequestObject{OrgId: org, PluginId: pluginID, Body: &body})
	require.NoError(t, err)
	require.Equal(t, CheckPluginAuthorization200JSONResponse{Allowed: true}, response)

	body.Permission = "module.version.unpin"
	_, err = server.CheckPluginAuthorization(authenticatedPluginContext(t, service), CheckPluginAuthorizationRequestObject{OrgId: org, PluginId: pluginID, Body: &body})
	require.Error(t, err, "Core permissions must never cross the plugin-local policy boundary")
	body.Permission = permission
	body.SubjectId = ref.Ref(uuid.New())
	_, err = server.CheckPluginAuthorization(authenticatedPluginContext(t, service), CheckPluginAuthorizationRequestObject{OrgId: org, PluginId: pluginID, Body: &body})
	require.Error(t, err, "a fresh proof must never accept a caller-selected subject")
}

func TestPluginAuthorizationRejectsResourceFromAnotherOrganizationBeforeIAM(t *testing.T) {
	const pluginID = "dev.example.workflow"
	permission := "plugin." + pluginID + ".rollout-design"
	service := userid.NewServiceUserTokenId().String()
	item := plugins.Installation{
		ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test",
		Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, ServiceUserID: service, AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		ServiceGrants:         []plugins.ServiceGrant{{Action: pluginAuthorizationCheckAction, Permission: permission, Scope: pluginOrganizationScope}},
		AuthorizationPolicies: []plugins.AuthorizationPolicy{{Permission: permission, Scopes: []string{"project"}}},
		Operations:            []plugins.Operation{{ID: "read", Method: http.MethodGet, Path: "/orgs/{orgId}/workflows", ServiceActions: []string{pluginAuthorizationCheckAction}}},
	}
	// The authorization.check grant itself must be covered by policy. Add the
	// organization scope without making it valid for the requested permission.
	item.AuthorizationPolicies[0].Scopes = []string{pluginOrganizationScope, "project"}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	ctrl := gomock.NewController(t)
	db := mockmodel.NewMockDatabaser(ctrl)
	project := uuid.New()
	db.EXPECT().GetProjectByUuid(gomock.Any(), nil, "acme", project, model.GetModeDefault).Return(nil, model.NewErrNotFound("project not found"))
	server := Server{Database: db, IamClient: mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl), PluginGateway: &plugins.Gateway{Registry: registry}}
	body := decodePluginAuthorizationBody(t, map[string]any{
		"permission": permission,
		"resources":  []map[string]string{{"type": "project", "id": project.String()}},
		"proof":      map[string]any{"operation_id": "read", "idempotency_key": "request-1", "assertion": "unused"},
	})
	_, err = server.CheckPluginAuthorization(authenticatedPluginContext(t, service), CheckPluginAuthorizationRequestObject{OrgId: "acme", PluginId: pluginID, Body: &body})
	require.Error(t, err)
}

func TestPluginAuthorizationGrantBoundsAreExactAndHaveNoCallerActor(t *testing.T) {
	project, environment := uuid.New(), uuid.New()
	intent := model.ExternalOperationIntent{Environments: []model.ExternalOperationEnvironment{{ProjectID: project, EnvironmentID: environment}}}
	require.True(t, resourcesWithinGrant([]pluginAuthorizationResource{{scope: "organization", id: "acme"}, {scope: "project", id: project.String(), projectID: project}, {scope: "environment", id: environment.String(), projectID: project}}, intent))
	require.False(t, resourcesWithinGrant([]pluginAuthorizationResource{{scope: "project", id: uuid.NewString()}}, intent))
	require.False(t, resourcesWithinGrant([]pluginAuthorizationResource{{scope: "environment", id: environment.String(), projectID: uuid.New()}}, intent))

	const pluginID = "dev.example.workflow"
	controller, subject := uuid.New(), uuid.New()
	checkPermission := "plugin." + pluginID + ".rollout-read"
	requestedPermission := "plugin." + pluginID + ".rollout-approve-environment"
	item := plugins.Installation{
		ID:            pluginID,
		ServiceGrants: []plugins.ServiceGrant{{Action: pluginAuthorizationCheckAction, Permission: checkPermission, Scope: pluginOrganizationScope}},
		AuthorizationPolicies: []plugins.AuthorizationPolicy{
			{Permission: checkPermission, Scopes: []string{pluginOrganizationScope}},
			{Permission: requestedPermission, Scopes: []string{"environment"}},
		},
	}
	grant := &model.ExternalOperationGrant{ActorID: controller, Intent: intent, State: "released"}
	resources := []pluginAuthorizationResource{{scope: "environment", id: environment.String(), projectID: project}}
	checks := []orchestratoriam.ResourcePermissionCheck{environmentCheck(environment, requestedPermission)}
	ctrl := gomock.NewController(t)
	iam := mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl)
	iam.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{UserId: controller, Checks: []orchestratoriam.ResourcePermissionCheck{orgCheck("acme", checkPermission)}}).Return(&orchestratoriam.InternalAuthorizeResponse{HTTPResponse: &http.Response{StatusCode: http.StatusNoContent}}, nil)
	iam.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{UserId: subject, Checks: checks}).Return(&orchestratoriam.InternalAuthorizeResponse{HTTPResponse: &http.Response{StatusCode: http.StatusNoContent}}, nil)
	allowed, err := (&Server{IamClient: iam}).pluginGrantAuthorizationDecision(t.Context(), "acme", item, grant, resources, checks, &subject)
	require.NoError(t, err)
	require.True(t, allowed, "grant authority derives its controller and accepts only a comparison subject")

	e, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "plugin-test", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	e.Use(middleware.EchoCtxMiddleware)
	(&Server{Logger: zap.NewNop()}).MapRoutes(e)
	request := httptest.NewRequest(http.MethodPost, "/orgs/acme/plugins/dev.example.workflow/authorization-checks", strings.NewReader(`{"permission":"plugin.dev.example.workflow.rollout-read","resources":[{"type":"organization","id":"acme"}],"operation_id":"`+uuid.NewString()+`","grant_fence":1,"actor_id":"`+uuid.NewString()+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("From", uuid.NewString())
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
}

func decodePluginAuthorizationBody(t *testing.T, value any) PluginAuthorizationCheckRequest {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var body PluginAuthorizationCheckRequest
	require.NoError(t, json.Unmarshal(encoded, &body))
	return body
}

func authenticatedPluginContext(t *testing.T, service string) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), hecho.ContextKeyUserID, service)
}
