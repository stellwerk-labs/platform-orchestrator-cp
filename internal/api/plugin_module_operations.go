package api

import (
	"context"
	"encoding/json"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
)

func (s *Server) ApplyPluginModuleOperation(ctx context.Context, request ApplyPluginModuleOperationRequestObject) (ApplyPluginModuleOperationResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil || request.Body.GrantFence <= 0 {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact fenced Module operation is required.")
	}
	action := string(request.Body.Action)
	policy, ok := item.ServiceGrant("module." + action)
	if !ok || policy.Scope != pluginOrganizationScope {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "An organization-scoped Module service grant is required.")
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	safetyReserve := action == "reserve" && grant.State == string(PluginOperationGrantStateActive) && grant.Revision == request.Body.GrantFence && grant.SafetyActivation
	safety := action == "release" || action == "restore" || safetyReserve
	if err := pluginFeatureAllowed(descriptor, policy.RequiredFeature, true, safety); err != nil {
		return nil, err
	}
	if err := s.pluginAuthorize(ctx, grant.ActorID, []orchestratoriam.ResourcePermissionCheck{orgCheck(request.OrgId, policy.Permission)}); err != nil {
		return nil, err
	}
	command := model.ExternalModuleCommand{OrgID: request.OrgId, PluginID: request.PluginId, OperationID: request.OperationId, ActorID: grant.ActorID, RequestID: request.Body.RequestId, Action: action, ExpectedFence: request.Body.ExpectedFence, GrantFence: request.Body.GrantFence, Reason: request.Body.Reason, SafetyReserve: safetyReserve}
	for _, target := range request.Body.Targets {
		command.Targets = append(command.Targets, model.ExternalModuleTarget{ModuleUUID: target.ModuleUuid, VersionUUID: target.VersionUuid, ExpectedDefaultUUID: target.ExpectedDefaultUuid, ExpectedResourceVersion: target.ExpectedResourceVersion})
	}
	receipt, err := model.ApplyExternalModuleOperation(ctx, s.pluginEffectDatabase(request.OrgId, item, "module."+action, safety), command)
	if err != nil {
		return nil, pluginModelError(err)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	var response ApplyPluginModuleOperation200JSONResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, err
	}
	return response, nil
}
