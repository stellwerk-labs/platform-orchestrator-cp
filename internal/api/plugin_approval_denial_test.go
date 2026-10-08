package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/herrors"
	iam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func TestApprovalDenialDistinguishesControllerAndSubject(t *testing.T) {
	for _, test := range []struct {
		name                string
		controller, subject int
		code                string
	}{
		{"allowed", 204, 204, ""},
		{"disabled-or-lost-scope", 204, 403, "plugin_subject_forbidden"},
		{"removed-identity", 204, 401, "plugin_subject_forbidden"},
		{"controller-denial", 403, 403, ""},
		{"controller-unavailable", 503, 403, "authorization_unavailable"},
		{"subject-unavailable", 204, 503, "authorization_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, subject := uuid.New(), uuid.New()
			calls := []uuid.UUID{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body iam.InternalAuthorizeBody
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				calls = append(calls, body.UserId)
				if body.UserId == controller {
					w.WriteHeader(test.controller)
				} else {
					assert.Equal(t, subject, body.UserId)
					w.WriteHeader(test.subject)
				}
			}))
			defer server.Close()
			client, err := iam.NewClientWithResponses(server.URL)
			require.NoError(t, err)
			permission := "plugin.dev.example.workflow.rollout-read"
			item := plugins.Installation{ID: "dev.example.workflow", ServiceGrants: []plugins.ServiceGrant{{Action: pluginAuthorizationCheckAction, Permission: permission, Scope: pluginOrganizationScope}}, AuthorizationPolicies: []plugins.AuthorizationPolicy{{Permission: permission, Scopes: []string{pluginOrganizationScope}}}}
			grant := &model.ExternalOperationGrant{ActorID: controller}
			allowed, err := (&Server{IamClient: client}).pluginGrantAuthorizationDecision(t.Context(), "acme", item, grant, []pluginAuthorizationResource{{scope: "organization", id: "acme"}}, []iam.ResourcePermissionCheck{orgCheck("acme", permission)}, &subject)
			if test.code != "" {
				var failure *herrors.PlatformOrchestratorError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, test.code, failure.Code)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.controller == 204 && test.subject == 204, allowed)
			}
			if test.controller != 204 {
				require.Equal(t, []uuid.UUID{controller}, calls, "controller denial must never become a subject decision")
			} else {
				require.Equal(t, []uuid.UUID{controller, subject}, calls)
			}
		})
	}
}

func TestFinalApprovalRecheckReturnsOnlyAuthoritativeApproverDenial(t *testing.T) {
	for _, status := range []int{204, 401, 403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			approver, environment := uuid.New(), uuid.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body iam.InternalAuthorizeBody
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, approver, body.UserId)
				w.WriteHeader(status)
			}))
			defer server.Close()
			client, err := iam.NewClientWithResponses(server.URL)
			require.NoError(t, err)
			dispatch := model.ExternalDispatch{OrgID: "acme", EnvironmentUUID: environment, Approval: &model.ExternalDispatchApproval{ActorID: approver, Scope: "environment"}, ApprovedPins: map[string]uuid.UUID{"module": uuid.Nil}, TargetVersions: map[string]uuid.UUID{"module": uuid.New()}}
			err = (&Server{IamClient: client}).authorizePluginDispatchApproval(t.Context(), plugins.Installation{OrganizationApprovalPolicies: map[string]map[string]string{"acme": {"environment": "plugin.dev.example.workflow.approve"}}}, dispatch)
			if status == 204 {
				require.NoError(t, err)
			} else {
				var failure *herrors.PlatformOrchestratorError
				require.ErrorAs(t, err, &failure)
				code := "plugin_approval_forbidden"
				if status == 503 {
					code = "authorization_unavailable"
				}
				require.Equal(t, code, failure.Code)
			}
		})
	}
}

func TestApprovalPoliciesApplyPerOrganization(t *testing.T) {
	approver, project, environment := uuid.New(), uuid.New(), uuid.New()
	asked := []iam.ResourcePermissionCheck{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body iam.InternalAuthorizeBody
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, approver, body.UserId)
		asked = append(asked, body.Checks...)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := iam.NewClientWithResponses(server.URL)
	require.NoError(t, err)
	const environmentApproval, projectApproval = "plugin.dev.example.workflow.approve-environment", "plugin.dev.example.workflow.approve-project"
	item := plugins.Installation{OrganizationApprovalPolicies: map[string]map[string]string{
		"acme": {"environment": environmentApproval},
		"beta": {"project": projectApproval},
	}}
	approve := func(org, scope string) error {
		dispatch := model.ExternalDispatch{OrgID: org, ProjectUUID: project, EnvironmentUUID: environment, Approval: &model.ExternalDispatchApproval{ActorID: approver, Scope: scope},
			ApprovedPins: map[string]uuid.UUID{"module": uuid.Nil}, TargetVersions: map[string]uuid.UUID{"module": uuid.New()}}
		return (&Server{IamClient: client}).authorizePluginDispatchApproval(t.Context(), item, dispatch)
	}

	require.NoError(t, approve("acme", "environment"))
	require.NoError(t, approve("beta", "project"))
	require.Equal(t, []iam.ResourcePermissionCheck{environmentCheck(environment, environmentApproval), projectCheck(project, projectApproval)}, asked,
		"each organization's approval asks IAM for that organization's policy")
	for _, refused := range []struct{ org, scope string }{{"beta", "environment"}, {"acme", "project"}, {"gamma", "environment"}} {
		var failure *herrors.PlatformOrchestratorError
		require.ErrorAs(t, approve(refused.org, refused.scope), &failure, "%s %s", refused.org, refused.scope)
		require.Equal(t, "plugin_approval_invalid", failure.Code, "another organization's approval policy never applies in %s", refused.org)
	}
	require.Len(t, asked, 2, "a scope the organization does not declare never reaches IAM")
}
