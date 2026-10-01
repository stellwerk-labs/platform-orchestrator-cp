package api

import (
	"context"
	"encoding/json"
	"slices"

	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

// ControlPluginOperationGrant can only reduce existing authority. The controller
// is taken from the stored grant; no user assertion is minted or replayed.
func (s *Server) ControlPluginOperationGrant(ctx context.Context, request ControlPluginOperationGrantRequestObject) (ControlPluginOperationGrantResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact authority-reducing command is required.")
	}
	body := request.Body
	if body.Action != PluginGrantControlSuspend && body.Action != PluginGrantControlRelease {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Service control may only suspend or release existing authority.")
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	action := "operation." + string(body.Action)
	policy, ok := item.ServiceGrant(action)
	if !ok || !slices.Contains(grant.Intent.Actions, action) {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The frozen operation did not delegate this service control action.")
	}
	if err := pluginFeatureAllowed(descriptor, policy.RequiredFeature, true, true); err != nil {
		return nil, err
	}
	checks := []orchestratoriam.ResourcePermissionCheck{}
	if policy.Scope == pluginOrganizationScope {
		checks = append(checks, orgCheck(request.OrgId, policy.Permission))
	} else {
		for _, environment := range grant.Intent.Environments {
			checks = append(checks, environmentCheck(environment.EnvironmentID, policy.Permission))
		}
	}
	if err := s.pluginAuthorize(ctx, grant.ActorID, checks); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	result, err := model.ApplyExternalGrantCommand(ctx, s.pluginEffectDatabase(request.OrgId, item, action, true), model.ExternalGrantCommand{
		OrgID: request.OrgId, PluginID: request.PluginId, OperationID: request.OperationId,
		ActorID: grant.ActorID, SourceOperation: "service:" + action, SourceDigest: plugincontext.Digest(encoded),
		RequestID: body.RequestId, ExpectedRevision: body.GrantFence, Action: string(body.Action), Reason: body.Reason,
	})
	if err != nil {
		return nil, pluginModelError(err)
	}
	encoded, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var response ControlPluginOperationGrant200JSONResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, err
	}
	return response, nil
}
