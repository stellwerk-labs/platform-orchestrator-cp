package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/herrors"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

const pluginOrganizationScope = "organization"

func pluginBoundaryError(status int, code, message string) error {
	return &herrors.PlatformOrchestratorError{StatusCode: status, Code: code, Message: message}
}

func pluginModelError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return pluginBoundaryError(404, "plugin_operation_not_found", "The operation does not exist in this scope.")
	}
	if _, ok := model.IsErrNotFound(err); ok {
		return pluginBoundaryError(404, "plugin_resource_not_found", "A referenced resource does not exist in this scope.")
	}
	if _, ok := model.IsErrBadRequest(err); ok {
		return pluginBoundaryError(400, "plugin_request_invalid", "The operation request is invalid.")
	}
	if _, ok := model.IsErrConflict(err); ok {
		return pluginBoundaryError(409, "plugin_operation_conflict", "The operation changed or its exact request identity was already used.")
	}
	return err
}

// pluginInstallationCaller only accepts the configured service principal after
// Gateway authentication. Neither the legacy organization-token shortcut nor
// the internal-system identity can authorize this boundary.
func (s *Server) pluginInstallationCaller(ctx context.Context, org, plugin string, checker bool) (plugins.Installation, plugins.Descriptor, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return plugins.Installation{}, plugins.Descriptor{}, err
	}
	if s.PluginGateway == nil || s.PluginGateway.Registry == nil {
		return plugins.Installation{}, plugins.Descriptor{}, pluginBoundaryError(404, "plugin_not_installed", "The plugin is not installed.")
	}
	item, descriptor, found := s.PluginGateway.Registry.Installation(ctx, org, plugin)
	identity := item.ServiceUserID
	if checker {
		identity = item.AdmissionCheckerID
	}
	if !found || identity == "" || actor.String() != identity {
		return plugins.Installation{}, plugins.Descriptor{}, pluginBoundaryError(403, "plugin_service_forbidden", "The configured plugin service identity is required.")
	}
	middleware.SetAuthCheckedCtx(ctx)
	return item, descriptor, nil
}

// pluginAuthorize does not use innerCheck: that older helper intentionally
// supports legacy org tokens and the internal-system bypass. Neither is valid
// for a delegated external operation. IAM remains authoritative for enabled
// identity and exact scope, including on an idempotent replay.
func (s *Server) pluginAuthorize(ctx context.Context, actor uuid.UUID, checks []orchestratoriam.ResourcePermissionCheck) error {
	allowed, err := s.pluginAuthorizationDecision(ctx, actor, checks)
	if err != nil {
		return err
	}
	if !allowed {
		return pluginBoundaryError(403, "plugin_actor_forbidden", "The actor no longer has the required capability and scope.")
	}
	return nil
}

func (s *Server) pluginAuthorizationDecision(ctx context.Context, actor uuid.UUID, checks []orchestratoriam.ResourcePermissionCheck) (bool, error) {
	if actor == uuid.Nil || actor.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" || s.IamClient == nil || len(checks) == 0 {
		return false, pluginBoundaryError(403, "plugin_actor_forbidden", "A scoped actor authorization is required.")
	}
	response, err := s.IamClient.InternalAuthorizeWithResponse(ctx, orchestratoriam.InternalAuthorizeBody{UserId: actor, Checks: checks})
	if err != nil || response == nil {
		return false, pluginBoundaryError(503, "authorization_unavailable", "Current authorization could not be verified. Retry after IAM recovers.")
	}
	if response.StatusCode() == http.StatusForbidden || response.StatusCode() == http.StatusUnauthorized {
		return false, nil
	}
	if response.StatusCode() != http.StatusNoContent {
		return false, pluginBoundaryError(503, "authorization_unavailable", "Current authorization could not be verified.")
	}
	return true, nil
}

func (s *Server) CheckPluginOperationDispatch(ctx context.Context, request CheckPluginOperationDispatchRequestObject) (CheckPluginOperationDispatchResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, true)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact dispatch check is required.")
	}
	action := string(request.Body.Action)
	policy, ok := item.ServiceGrant(action)
	if !ok {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation has no grant for this service action.")
	}
	safety := action == "external-deployment.rollback"
	allowed := descriptor.State == plugins.StateAvailable || safety && (descriptor.State == plugins.StateNotEnabled || descriptor.State == plugins.StateNotEntitled)
	if !allowed {
		return nil, pluginBoundaryError(409, "plugin_"+string(descriptor.State), "The plugin cannot perform this operation in its current availability state.")
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, pluginBoundaryError(404, "plugin_operation_not_found", "The operation grant does not exist in this scope.")
	}
	if err != nil {
		return nil, err
	}
	check := environmentCheck(request.Body.EnvironmentUuid, policy.Permission)
	if policy.Scope == pluginOrganizationScope {
		check = orgCheck(request.OrgId, policy.Permission)
	}
	if err := s.pluginAuthorize(ctx, grant.ActorID, []orchestratoriam.ResourcePermissionCheck{check}); err != nil {
		return nil, err
	}
	dispatch := model.ExternalDispatch{OrgID: request.OrgId, PluginID: request.PluginId, OperationID: request.OperationId, ActorID: grant.ActorID, Fence: request.Body.Fence, Action: action, DispatchID: request.Body.DispatchId, ProjectUUID: request.Body.ProjectUuid, EnvironmentUUID: request.Body.EnvironmentUuid, RequestDigest: request.Body.RequestDigest}
	recorded, err := model.GetExternalDispatch(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId, dispatch.DispatchID)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if err := s.authorizePluginDispatchApproval(ctx, item, *recorded); err != nil {
		return nil, err
	}
	if err := model.CheckExternalDispatch(ctx, s.Database, dispatch); err != nil {
		if _, ok := model.IsErrConflict(err); ok {
			return nil, pluginBoundaryError(409, "plugin_dispatch_conflict", "The exact dispatch is no longer authorized at this operation fence.")
		}
		if _, ok := model.IsErrBadRequest(err); ok {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "The dispatch check is invalid.")
		}
		return nil, err
	}
	return CheckPluginOperationDispatch200JSONResponse{OrgId: dispatch.OrgID, PluginId: dispatch.PluginID, OperationId: dispatch.OperationID, ActorId: dispatch.ActorID, Fence: dispatch.Fence, Action: dispatch.Action, DispatchId: dispatch.DispatchID, ProjectUuid: dispatch.ProjectUUID, EnvironmentUuid: dispatch.EnvironmentUUID, RequestDigest: dispatch.RequestDigest}, nil
}
