package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

const ordinaryDeploymentReadPermission = "deployment_read"

func (s *Server) GetPluginDeploymentMaterial(ctx context.Context, request GetPluginDeploymentMaterialRequestObject) (GetPluginDeploymentMaterialResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	middleware.SetAuthCheckedCtx(ctx)
	if s.PluginGateway == nil || s.PluginGateway.Registry == nil {
		return nil, pluginBoundaryError(503, "deployment_material_unavailable", "The protected Deployment material service is not configured.")
	}
	// This is a Core user endpoint. Even an accidentally overprivileged plugin
	// or admission-checker principal must not receive plaintext material.
	for _, descriptor := range s.PluginGateway.Registry.List(ctx, request.OrgId) {
		installation, _, _ := s.PluginGateway.Registry.Installation(ctx, request.OrgId, descriptor.ID)
		if actor.String() == installation.ServiceUserID || actor.String() == installation.AdmissionCheckerID {
			return nil, pluginBoundaryError(403, "deployment_material_forbidden", "Plugin service identities cannot retrieve decrypted Deployment material.")
		}
	}
	allowed, err := s.pluginAuthorizationDecision(ctx, actor, []iamapi.ResourcePermissionCheck{environmentCheck(request.EnvironmentUuid, ordinaryDeploymentReadPermission)})
	if err != nil {
		return nil, err
	}
	if !allowed {
		// Historical Environment IAM relationships may have been removed. An
		// independently held organization grant still authorizes retained data;
		// a plugin grant or a lookup failure never substitutes for that grant.
		if err := s.pluginAuthorize(ctx, actor, []iamapi.ResourcePermissionCheck{orgCheck(request.OrgId, ordinaryDeploymentReadPermission)}); err != nil {
			return nil, err
		}
	}
	if _, _, found := s.PluginGateway.Registry.Installation(ctx, request.OrgId, request.PluginId); !found {
		return nil, pluginBoundaryError(404, "plugin_not_installed", "The plugin's retained installation metadata is not available in this organization.")
	}
	action, extension := "", ""
	switch request.MaterialKind {
	case PluginDeploymentMaterialLogs:
		action, extension = plugins.DataPlaneLogsRead, "log"
	case PluginDeploymentMaterialOutputs:
		action, extension = plugins.DataPlaneOutputsRead, "json"
	default:
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Choose logs or outputs for an exact Deployment.")
	}
	result, err := s.PluginDataPlane.ReadMaterial(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: actor, Action: action, RequestID: "material-" + uuid.NewString(), ProjectUUID: request.ProjectUuid, EnvironmentUUID: request.EnvironmentUuid, DeploymentID: request.DeploymentId})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if result.Status != http.StatusOK {
		status := result.Status
		if status != http.StatusNotFound && status != http.StatusConflict && status != http.StatusRequestEntityTooLarge && status != http.StatusServiceUnavailable {
			status = http.StatusBadGateway
		}
		return nil, pluginBoundaryError(status, "deployment_material_unavailable", "The authorized Deployment material could not be retrieved. Check its retention and encryption service state.")
	}
	return GetPluginDeploymentMaterial200TextResponse{Body: string(result.Body), Headers: GetPluginDeploymentMaterial200ResponseHeaders{ContentDisposition: ref.Ref(fmt.Sprintf("attachment; filename=\"%s.%s\"", request.DeploymentId.String(), extension)), CacheControl: ref.Ref("no-store"), XContentTypeOptions: ref.Ref("nosniff")}}, nil
}
