package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	iamauthz "github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

// pluginManagerCaller authorizes a platform-scoped Plugin Manager request.
// IAM decides: only operators configured there hold the platform
// permissions; organization roles and plugin workloads never qualify.
func (s *Server) pluginManagerCaller(ctx context.Context, permission string) (uuid.UUID, error) {
	if s.PluginManager == nil {
		return uuid.Nil, pluginBoundaryError(404, "plugin_host_disabled", "The plugin host is disabled on this Control Plane.")
	}
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	response, err := s.IamClient.InternalAuthorizeWithResponse(ctx, orchestratoriam.InternalAuthorizeBody{
		UserId: actor, Checks: []orchestratoriam.ResourcePermissionCheck{iamauthz.PlatformCheck(permission)},
	})
	if err != nil {
		return uuid.Nil, errors.Wrap(err, "failed to check the platform permission")
	}
	switch response.StatusCode() {
	case http.StatusNoContent:
	case http.StatusForbidden:
		return uuid.Nil, pluginBoundaryError(403, "plugin_manager_forbidden", "The platform permission "+permission+" is required.")
	default:
		return uuid.Nil, errors.Errorf("unexpected status code when checking the platform permission: %s", response.Status())
	}
	middleware.SetAuthCheckedCtx(ctx)
	return actor, nil
}

func (s *Server) pluginManagerCommand(ctx context.Context, idempotencyKey string, expectedRevision int64, reason string) (pluginmanager.Command, error) {
	actor, err := s.pluginManagerCaller(ctx, iamauthz.PermissionPluginManagerWrite)
	if err != nil {
		return pluginmanager.Command{}, err
	}
	command := pluginmanager.Command{Actor: actor, IdempotencyKey: idempotencyKey, ExpectedRevision: expectedRevision, Reason: reason}
	if c := middleware.GetEchoCtx(ctx); c != nil {
		command.RequestID = c.Response().Header().Get(echo.HeaderXRequestID)
	}
	return command, nil
}

func pluginManagerError(err error) error {
	if bad, ok := model.IsErrBadRequest(err); ok {
		return pluginBoundaryError(400, ref.DerefOr(nonEmptyString(bad.Code), "plugin_manager_request_invalid"), bad.Message)
	}
	if missing, ok := model.IsErrNotFound(err); ok {
		return pluginBoundaryError(404, ref.DerefOr(nonEmptyString(missing.Code), "plugin_manager_not_found"), missing.Message)
	}
	if conflict, ok := model.IsErrConflict(err); ok {
		return pluginBoundaryError(409, ref.DerefOr(nonEmptyString(conflict.Code), "plugin_manager_conflict"), conflict.Message)
	}
	return err
}

// convertJSON maps a Plugin Manager view onto its generated API type.
func convertJSON[T any](value any) (T, error) {
	var result T
	encoded, err := json.Marshal(value)
	if err != nil {
		return result, err
	}
	return result, json.Unmarshal(encoded, &result)
}

func pluginInstallationResult[T any](installation pluginmanager.Installation, err error) (T, error) {
	if err != nil {
		var zero T
		return zero, pluginManagerError(err)
	}
	return convertJSON[T](installation)
}

func (s *Server) ListPluginPackages(ctx context.Context, _ ListPluginPackagesRequestObject) (ListPluginPackagesResponseObject, error) {
	if _, err := s.pluginManagerCaller(ctx, iamauthz.PermissionPluginManagerRead); err != nil {
		return nil, err
	}
	page, err := s.PluginManager.Packages(ctx)
	if err != nil {
		return nil, pluginManagerError(err)
	}
	return convertJSON[ListPluginPackages200JSONResponse](page)
}

func (s *Server) PushPluginPackage(ctx context.Context, request PushPluginPackageRequestObject) (PushPluginPackageResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "A package is required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	pkg, err := s.PluginManager.PushPackage(ctx, command, request.Body.ManifestSha256, request.Body.Manifest)
	if err != nil {
		return nil, pluginManagerError(err)
	}
	return convertJSON[PushPluginPackage201JSONResponse](pkg)
}

func (s *Server) ListPluginInstallations(ctx context.Context, _ ListPluginInstallationsRequestObject) (ListPluginInstallationsResponseObject, error) {
	if _, err := s.pluginManagerCaller(ctx, iamauthz.PermissionPluginManagerRead); err != nil {
		return nil, err
	}
	page, err := s.PluginManager.Installations(ctx)
	if err != nil {
		return nil, pluginManagerError(err)
	}
	return convertJSON[ListPluginInstallations200JSONResponse](page)
}

func (s *Server) CreatePluginInstallation(ctx context.Context, request CreatePluginInstallationRequestObject) (CreatePluginInstallationResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "An installation is required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[CreatePluginInstallation201JSONResponse](s.PluginManager.CreateInstallation(ctx, command, request.Body.PackageSha256, request.Body.BackendUrl, ref.DerefOr(request.Body.UiOrigin, "")))
}

func (s *Server) GetPluginInstallation(ctx context.Context, request GetPluginInstallationRequestObject) (GetPluginInstallationResponseObject, error) {
	if _, err := s.pluginManagerCaller(ctx, iamauthz.PermissionPluginManagerRead); err != nil {
		return nil, err
	}
	return pluginInstallationResult[GetPluginInstallation200JSONResponse](s.PluginManager.Installation(ctx, request.InstallationId))
}

func (s *Server) SetPluginInstallationIdentity(ctx context.Context, request SetPluginInstallationIdentityRequestObject) (SetPluginInstallationIdentityResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "The service identities are required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[SetPluginInstallationIdentity200JSONResponse](s.PluginManager.SetIdentity(ctx, command, request.InstallationId, request.Body.ServiceUserId.String(), request.Body.AdmissionCheckerId.String()))
}

func (s *Server) TransitionPluginInstallation(ctx context.Context, request TransitionPluginInstallationRequestObject) (TransitionPluginInstallationResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "The expected revision and a reason are required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[TransitionPluginInstallation200JSONResponse](s.PluginManager.Transition(ctx, command, request.InstallationId, pluginmanager.Action(request.InstallationAction)))
}

func (s *Server) UpgradePluginInstallation(ctx context.Context, request UpgradePluginInstallationRequestObject) (UpgradePluginInstallationResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "The target package is required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[UpgradePluginInstallation200JSONResponse](s.PluginManager.Upgrade(ctx, command, request.InstallationId, request.Body.PackageSha256))
}

func (s *Server) PurgePluginInstallation(ctx context.Context, request PurgePluginInstallationRequestObject) (PurgePluginInstallationResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "The expected revision and a reason are required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[PurgePluginInstallation200JSONResponse](s.PluginManager.Transition(ctx, command, request.InstallationId, pluginmanager.ActionPurge))
}

func (s *Server) PutPluginEnablement(ctx context.Context, request PutPluginEnablementRequestObject) (PutPluginEnablementResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "An enablement is required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[PutPluginEnablement200JSONResponse](s.PluginManager.PutEnablement(ctx, command, request.InstallationId, request.OrgId, string(request.Body.State),
		ref.DerefOr(request.Body.Timezone, ""), ref.DerefOr(request.Body.ApprovalPolicies, map[string]string{})))
}

func (s *Server) RemovePluginEnablement(ctx context.Context, request RemovePluginEnablementRequestObject) (RemovePluginEnablementResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "The expected revision and a reason are required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	return pluginInstallationResult[RemovePluginEnablement200JSONResponse](s.PluginManager.RemoveEnablement(ctx, command, request.InstallationId, request.OrgId))
}

func (s *Server) ImportPluginEntitlementBundle(ctx context.Context, request ImportPluginEntitlementBundleRequestObject) (ImportPluginEntitlementBundleResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_manager_request_invalid", "A signed entitlement bundle is required.")
	}
	command, err := s.pluginManagerCommand(ctx, request.Params.IdempotencyKey, request.Body.ExpectedRevision, request.Body.Reason)
	if err != nil {
		return nil, err
	}
	report, err := s.PluginManager.ImportEntitlementBundle(ctx, command, request.Body.Bundle)
	if err != nil {
		return nil, pluginManagerError(err)
	}
	return convertJSON[ImportPluginEntitlementBundle200JSONResponse](report)
}

func (s *Server) ListPluginAuditEvents(ctx context.Context, request ListPluginAuditEventsRequestObject) (ListPluginAuditEventsResponseObject, error) {
	if _, err := s.pluginManagerCaller(ctx, iamauthz.PermissionPluginManagerRead); err != nil {
		return nil, err
	}
	page, err := s.PluginManager.AuditEvents(ctx, ref.DerefOr(request.Params.After, 0), ref.DerefOr(request.Params.Limit, 100))
	if err != nil {
		return nil, pluginManagerError(err)
	}
	return convertJSON[ListPluginAuditEvents200JSONResponse](page)
}
