package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
	dpapi "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

func (s *Server) ApplyPluginPinOperation(ctx context.Context, request ApplyPluginPinOperationRequestObject) (ApplyPluginPinOperationResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil || len(request.Body.Targets) == 0 || len(request.Body.Targets) > 1000 {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "A bounded exact Pin operation is required.")
	}
	body := request.Body
	begin := body.Action == PluginPinBegin
	if begin != (body.Proof != nil) {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Only beginning an override requires a fresh explicit user command.")
	}
	action := "pin.override." + string(body.Action)
	policy, exists := item.ServiceGrant(action)
	if !exists {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not declare this Pin service action.")
	}
	allowed := descriptor.State == plugins.StateAvailable || body.Action != PluginPinBegin && (descriptor.State == plugins.StateNotEnabled || descriptor.State == plugins.StateNotEntitled)
	if !allowed {
		return nil, pluginBoundaryError(409, "plugin_"+string(descriptor.State), "The plugin cannot perform this Pin operation in its current availability state.")
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, item.ID, request.OperationId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if grant.Revision != body.GrantFence || !slices.Contains(grant.Intent.Actions, action) {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The Pin action is outside the current operation grant.")
	}
	actor := grant.ActorID
	if begin {
		claims, err := s.PluginGateway.VerifyServiceDelegation(ctx, request.OrgId, item.ID, item.ServiceUserID, action, pluginProofFromAPI(*body.Proof))
		if err != nil {
			return nil, pluginGatewayError(err)
		}
		actor, err = uuid.Parse(claims.ActorID)
		if err != nil || actor == uuid.Nil {
			return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "A scoped user must explicitly authorize this override.")
		}
	}
	command := model.ExternalPinCommand{OrgID: request.OrgId, PluginID: item.ID, OperationID: grant.OperationID, GrantFence: body.GrantFence, ActorID: actor, Action: string(body.Action), RequestID: body.RequestId, Reason: body.Reason}
	checks := []orchestratoriam.ResourcePermissionCheck{}
	pins := make([]model.EnvironmentModuleVersionPin, 0, len(body.Targets))
	for _, target := range body.Targets {
		pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, target.PinId, model.GetModeDefault)
		if err != nil {
			return nil, pluginModelError(err)
		}
		if !slices.Contains(grant.Intent.Environments, model.ExternalOperationEnvironment{ProjectID: pin.ProjectUUID, EnvironmentID: pin.EnvironmentUUID}) || grant.Intent.TargetVersions[pin.ModuleUUID.String()] == uuid.Nil {
			return nil, pluginBoundaryError(403, "plugin_service_forbidden", "Pin targets must belong to the exact frozen operation scope.")
		}
		pins = append(pins, *pin)
		checks = append(checks, environmentCheck(pin.EnvironmentUUID, policy.Permission))
		entry := model.ExternalPinTarget{PinID: target.PinId, ExpectedResourceVersion: target.ExpectedResourceVersion}
		if target.TargetVersionUuid != nil {
			entry.TargetVersionUUID = *target.TargetVersionUuid
		}
		if target.DeploymentId != nil {
			entry.DeploymentID = *target.DeploymentId
		}
		command.Targets = append(command.Targets, entry)
	}
	if policy.Scope == pluginOrganizationScope {
		checks = []orchestratoriam.ResourcePermissionCheck{orgCheck(request.OrgId, policy.Permission)}
	}
	if err := s.pluginAuthorize(ctx, actor, checks); err != nil {
		return nil, err
	}
	// Fetch authoritative evidence without holding a Core grant/Environment
	// lock. DP's receipt read serializes with admission, whose callback needs
	// those locks. The model subsequently rechecks current Pin revisions.
	evidence := map[uuid.UUID]model.ExternalPinEvidence{}
	if body.Action == PluginPinReconcile || body.Action == PluginPinRestore {
		for i, pin := range pins {
			deployment := command.Targets[i].DeploymentID
			if deployment == uuid.Nil {
				return nil, pluginBoundaryError(400, "plugin_request_invalid", "Reconciliation requires an exact Deployment identity.")
			}
			resolved, err := s.resolvePluginPinEvidence(ctx, grant, pin, deployment, command.RequestID)
			if err != nil {
				return nil, err
			}
			evidence[pin.ID] = resolved
		}
	}
	result, err := model.ApplyExternalPinCommand(ctx, s.Database, command, func(_ context.Context, _ model.Tx, _ *model.ExternalOperationGrant, pin model.EnvironmentModuleVersionPin, deployment uuid.UUID) (model.ExternalPinEvidence, error) {
		resolved, exists := evidence[pin.ID]
		if !exists || resolved.DeploymentID != deployment {
			return model.ExternalPinEvidence{}, invalidPluginDeploymentReceipt()
		}
		return resolved, nil
	})
	if err != nil {
		return nil, pluginModelError(err)
	}
	response := make(ApplyPluginPinOperation200JSONResponse, 0, len(result))
	for _, pin := range result {
		response = append(response, modulePinToAPI(pin))
	}
	return response, nil
}

func (s *Server) resolvePluginPinEvidence(ctx context.Context, grant *model.ExternalOperationGrant, pin model.EnvironmentModuleVersionPin, deployment uuid.UUID, requestID string) (model.ExternalPinEvidence, error) {
	empty := model.ExternalPinEvidence{}
	environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, grant.OrgID, pin.EnvironmentUUID, model.GetModeDefault)
	if err != nil {
		return empty, pluginModelError(err)
	}
	if environment.ProjectUuid != pin.ProjectUUID {
		return empty, invalidPluginDeploymentReceipt()
	}
	ref := pluginDeploymentEnvironment{ProjectID: environment.ProjectId, EnvironmentID: environment.Id, ProjectUUID: environment.ProjectUuid, EnvironmentUUID: environment.Uuid}
	identity := "pin-evidence-" + plugincontext.Digest([]byte(requestID+":"+pin.ID.String()+":"+deployment.String()))
	result, err := s.readPluginDeploymentReceipt(ctx, grant.OrgID, grant.PluginID, grant.ActorID, ref, deployment, "", identity)
	if err != nil {
		return empty, err
	}
	if result.Status != http.StatusOK {
		return empty, invalidPluginDeploymentReceipt()
	}
	var receipt dpapi.ExternalDeploymentStatusResponse
	if err := json.Unmarshal(result.Body, &receipt); err != nil {
		return empty, invalidPluginDeploymentReceipt()
	}
	if receipt.OperationRef != grant.OperationID || receipt.DeploymentId != deployment || receipt.EnvironmentUuid != pin.EnvironmentUUID || receipt.ProjectUuid != pin.ProjectUUID {
		return empty, invalidPluginDeploymentReceipt()
	}
	evidence := model.ExternalPinEvidence{OperationID: receipt.OperationRef, EnvironmentUUID: receipt.EnvironmentUuid, ProjectUUID: receipt.ProjectUuid, DeploymentID: receipt.DeploymentId, Mode: string(receipt.Mode), Status: string(receipt.Status), ModuleVersions: map[uuid.UUID]uuid.UUID{}, EffectiveVersions: map[uuid.UUID]uuid.UUID{}}
	for key, value := range receipt.ModuleVersions {
		module, err := uuid.Parse(key)
		if err != nil || module == uuid.Nil || value == uuid.Nil {
			return empty, invalidPluginDeploymentReceipt()
		}
		evidence.ModuleVersions[module] = value
	}
	if receipt.Status != dpapi.ExternalDeploymentStatusSucceeded {
		return evidence, nil
	}
	body, err := json.Marshal(dpapi.ExternalDeploymentReadinessBody{Environments: []dpapi.ExternalEnvironmentRef{{ProjectId: ref.ProjectID, EnvId: ref.EnvironmentID, ProjectUuid: ref.ProjectUUID, EnvironmentUuid: ref.EnvironmentUUID}}})
	if err != nil {
		return empty, err
	}
	current, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: grant.OrgID, PluginID: grant.PluginID, ActorID: grant.ActorID, Action: "external-deployment.readiness", RequestID: identity, Body: body})
	if err != nil {
		return empty, pluginGatewayError(err)
	}
	var readiness dpapi.ExternalDeploymentReadinessResponse
	if current.Status != http.StatusOK || json.Unmarshal(current.Body, &readiness) != nil || len(readiness.Items) != 1 {
		return empty, invalidPluginDeploymentReceipt()
	}
	state := readiness.Items[0]
	if state.EnvironmentUuid != pin.EnvironmentUUID || state.ProjectUuid != pin.ProjectUUID || state.State != "ready" || state.EffectiveBaseDeploymentId == nil || *state.EffectiveBaseDeploymentId != deployment {
		return empty, pluginBoundaryError(409, "plugin_operation_conflict", "The Environment changed or is busy; Pin protection cannot be reconciled from stale evidence.")
	}
	for _, active := range state.ActiveModules {
		if active.ModuleUuid == nil || active.VersionUuid == nil {
			return empty, invalidPluginDeploymentReceipt()
		}
		evidence.EffectiveVersions[*active.ModuleUuid] = *active.VersionUuid
	}
	return evidence, nil
}
