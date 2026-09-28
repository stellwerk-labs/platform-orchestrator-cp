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

const pluginDeploymentReadAction = "external-deployment.read"

func (s *Server) SubmitPluginDeployment(ctx context.Context, request SubmitPluginDeploymentRequestObject) (SubmitPluginDeploymentResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact Deployment request is required.")
	}
	body := request.Body
	if err := validatePluginDeploymentSubmit(*body); err != nil {
		return nil, err
	}
	action := "external-deployment.admit"
	if body.Mode == PluginDeploymentSubmitModeRollback {
		action = "external-deployment.rollback"
	}
	grant, environment, err := s.authorizePluginEnvironmentOperation(ctx, request.OrgId, item, descriptor, request.OperationId, body.GrantFence, body.EnvironmentUuid, action, body.Mode == PluginDeploymentSubmitModeRollback)
	if err != nil {
		return nil, err
	}
	if grant.State != "active" {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Deployment admission requires an active operation.")
	}
	admission := dpapi.ExternalDeploymentAdmissionBody{OperationRef: grant.OperationID, Fence: grant.Revision, ProjectId: environment.ProjectID, EnvId: environment.EnvironmentID, ProjectUuid: environment.ProjectUUID, EnvironmentUuid: environment.EnvironmentUUID, Mode: dpapi.ExternalDeploymentAdmissionBodyMode(body.Mode)}
	targets := map[string]uuid.UUID{}
	if body.Plan != nil {
		admission.Plan = &dpapi.ExternalDeploymentAdmissionPlan{SnapshotId: body.Plan.SnapshotId, Fingerprint: body.Plan.Fingerprint, ExpectedModuleVersions: body.Plan.ModuleVersions}
		targets = body.Plan.ModuleVersions
	} else {
		// A plugin cannot choose an arbitrary historical rollback target. Only
		// DP's retained base of this operation's own forward effect is eligible.
		result, err := s.readPluginDeploymentReceipt(ctx, request.OrgId, item.ID, grant.ActorID, environment, body.Rollback.ForwardDeploymentId, "", "base-"+plugincontext.Digest([]byte(body.RequestId)))
		if err != nil {
			return nil, err
		}
		if result.Status != http.StatusOK {
			return SubmitPluginDeployment200JSONResponse{Status: result.Status, Body: result.Body}, nil
		}
		var receipt dpapi.ExternalDeploymentStatusResponse
		if err := json.Unmarshal(result.Body, &receipt); err != nil {
			return nil, invalidPluginDeploymentReceipt()
		}
		if receipt.DeploymentId != body.Rollback.ForwardDeploymentId || receipt.OperationRef != grant.OperationID || receipt.EnvironmentUuid != environment.EnvironmentUUID || receipt.ProjectUuid != environment.ProjectUUID || receipt.Mode != dpapi.ExternalDeploymentStatusModeDeploy || receipt.Status != dpapi.ExternalDeploymentStatusSucceeded {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Rollback requires this operation's successful forward Deployment and exact captured base.")
		}
		for module, version := range receipt.RollbackModuleVersions {
			if _, owned := grant.Intent.TargetVersions[module]; owned {
				targets[module] = version
			}
		}
		if len(targets) == 0 {
			return nil, invalidPluginDeploymentReceipt()
		}
		admission.Rollback = &dpapi.ExternalDeploymentAdmissionRollback{ForwardDeploymentId: receipt.DeploymentId}
	}
	encoded, err := json.Marshal(admission)
	if err != nil {
		return nil, err
	}
	dispatch := model.ExternalDispatch{OrgID: request.OrgId, PluginID: item.ID, OperationID: grant.OperationID, Fence: grant.Revision, ActorID: grant.ActorID, Action: action, DispatchID: body.RequestId, ProjectUUID: environment.ProjectUUID, EnvironmentUUID: environment.EnvironmentUUID, RequestDigest: plugincontext.Digest(encoded), TargetVersions: targets}
	if body.ApprovedPinBoundaries != nil {
		dispatch.ApprovedPins = *body.ApprovedPinBoundaries
	}
	if body.Approval != nil {
		dispatch.Approval = &model.ExternalDispatchApproval{ActorID: body.Approval.ActorId, Scope: string(body.Approval.Scope)}
	}
	if err := s.authorizePluginDispatchApproval(ctx, item, dispatch); err != nil {
		return nil, err
	}
	if err := model.RecordExternalDispatch(ctx, s.Database, dispatch); err != nil {
		return nil, pluginModelError(err)
	}
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: item.ID, ActorID: grant.ActorID, Action: "external-deployment.admit", RequestID: body.RequestId, Body: encoded})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	return SubmitPluginDeployment200JSONResponse{Status: result.Status, Body: result.Body}, nil
}

func validatePluginDeploymentSubmit(body PluginDeploymentSubmitRequest) error {
	invalid := pluginBoundaryError(400, "plugin_request_invalid", "Supply one exact planned deployment or captured-base rollback, with matching approval evidence.")
	if body.GrantFence <= 0 || body.EnvironmentUuid == uuid.Nil || body.RequestId == "" || len(body.RequestId) > 128 || (body.Approval == nil) != (body.ApprovedPinBoundaries == nil) {
		return invalid
	}
	switch body.Mode {
	case PluginDeploymentSubmitModeDeploy:
		if body.Plan == nil || body.Rollback != nil || body.Plan.SnapshotId == uuid.Nil || len(body.Plan.ModuleVersions) == 0 {
			return invalid
		}
	case PluginDeploymentSubmitModeRollback:
		if body.Plan != nil || body.Rollback == nil || body.Rollback.ForwardDeploymentId == uuid.Nil {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

func (s *Server) authorizePluginEnvironmentOperation(ctx context.Context, org string, item plugins.Installation, descriptor plugins.Descriptor, operation uuid.UUID, fence int64, environmentID uuid.UUID, action string, safety bool) (*model.ExternalOperationGrant, pluginDeploymentEnvironment, error) {
	empty := pluginDeploymentEnvironment{}
	allowed := descriptor.State == plugins.StateAvailable || safety && (descriptor.State == plugins.StateNotEnabled || descriptor.State == plugins.StateNotEntitled)
	if !allowed {
		return nil, empty, pluginBoundaryError(409, "plugin_"+string(descriptor.State), "The plugin cannot perform this operation in its current availability state.")
	}
	policy, ok := item.ServiceGrant(action)
	if !ok {
		return nil, empty, pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not declare this service action.")
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, org, item.ID, operation)
	if err != nil {
		return nil, empty, pluginModelError(err)
	}
	environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, org, environmentID, model.GetModeDefault)
	if err != nil {
		return nil, empty, pluginModelError(err)
	}
	bound := model.ExternalOperationEnvironment{ProjectID: environment.ProjectUuid, EnvironmentID: environment.Uuid}
	if fence <= 0 || grant.Revision != fence || !slices.Contains(grant.Intent.Actions, action) || !slices.Contains(grant.Intent.Environments, bound) {
		return nil, empty, pluginBoundaryError(409, "plugin_operation_conflict", "The action or Environment is outside the current frozen operation grant.")
	}
	check := environmentCheck(environment.Uuid, policy.Permission)
	if policy.Scope == pluginOrganizationScope {
		check = orgCheck(org, policy.Permission)
	}
	if err := s.pluginAuthorize(ctx, grant.ActorID, []orchestratoriam.ResourcePermissionCheck{check}); err != nil {
		return nil, empty, err
	}
	return grant, pluginDeploymentEnvironment{ProjectID: environment.ProjectId, EnvironmentID: environment.Id, ProjectUUID: environment.ProjectUuid, EnvironmentUUID: environment.Uuid}, nil
}

func (s *Server) authorizePluginDispatchApproval(ctx context.Context, item plugins.Installation, dispatch model.ExternalDispatch) error {
	if dispatch.Approval == nil && dispatch.ApprovedPins == nil {
		return nil
	}
	if dispatch.Approval == nil || dispatch.ApprovedPins == nil || len(dispatch.ApprovedPins) != len(dispatch.TargetVersions) {
		return pluginBoundaryError(403, "plugin_approval_invalid", "Approval must cover every exact Module target and Pin activation boundary.")
	}
	permission, exists := item.DeploymentApprovalPolicies[dispatch.Approval.Scope]
	if !exists {
		return pluginBoundaryError(403, "plugin_approval_invalid", "The installation does not declare this approval scope.")
	}
	check := environmentCheck(dispatch.EnvironmentUUID, permission)
	if dispatch.Approval.Scope == "project" {
		check = projectCheck(dispatch.ProjectUUID, permission)
	}
	return s.pluginAuthorize(ctx, dispatch.Approval.ActorID, []orchestratoriam.ResourcePermissionCheck{check})
}

func (s *Server) readPluginDeploymentReceipt(ctx context.Context, org, plugin string, actor uuid.UUID, environment pluginDeploymentEnvironment, deployment uuid.UUID, dispatchID, requestID string) (*plugins.InvocationResult, error) {
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: org, PluginID: plugin, ActorID: actor, Action: pluginDeploymentReadAction, RequestID: requestID, ProjectUUID: environment.ProjectUUID, EnvironmentUUID: environment.EnvironmentUUID, DeploymentID: deployment, DispatchID: dispatchID})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	return result, nil
}

func invalidPluginDeploymentReceipt() error {
	return pluginBoundaryError(502, "plugin_response_invalid", "The Data Plane did not return verifiable exact-scope Deployment evidence.")
}

func (s *Server) ReconcilePluginDeployment(ctx context.Context, request ReconcilePluginDeploymentRequestObject) (ReconcilePluginDeploymentResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact dispatch recovery request is required.")
	}
	dispatch, err := model.GetExternalDispatch(ctx, s.Database, request.OrgId, item.ID, request.OperationId, request.DispatchId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	action := pluginDeploymentReadAction
	if request.Body.Withdraw {
		action = "external-deployment.withdraw"
	}
	grant, environment, err := s.authorizePluginEnvironmentOperation(ctx, request.OrgId, item, descriptor, request.OperationId, request.Body.GrantFence, dispatch.EnvironmentUUID, action, true)
	if err != nil {
		return nil, err
	}
	if request.Body.Withdraw {
		// Commit withdrawal before DP receipt acquisition. Holding a Core lock
		// across the HTTP request would deadlock DP's admission callback.
		if err := model.WithdrawExternalDispatch(ctx, s.Database, request.OrgId, item.ID, request.OperationId, request.DispatchId); err != nil {
			return nil, pluginModelError(err)
		}
	}
	result, err := s.readPluginDeploymentReceipt(ctx, request.OrgId, item.ID, grant.ActorID, environment, uuid.Nil, request.DispatchId, request.DispatchId)
	if err != nil {
		return nil, err
	}
	evidence := model.ExternalDispatchCompletion{EnvironmentUUID: dispatch.EnvironmentUUID, ProjectUUID: dispatch.ProjectUUID, RequestDigest: dispatch.RequestDigest}
	settle := false
	if result.Status == http.StatusNotFound && request.Body.Withdraw {
		var absent dpapi.Error
		if json.Unmarshal(result.Body, &absent) != nil || absent.Error != "dispatch_not_found" {
			return nil, invalidPluginDeploymentReceipt()
		}
		evidence.NoEffect, settle = true, true
	} else if result.Status == http.StatusOK {
		var receipt dpapi.ExternalDeploymentStatusResponse
		if err := json.Unmarshal(result.Body, &receipt); err != nil {
			return nil, invalidPluginDeploymentReceipt()
		}
		if receipt.DeploymentId == uuid.Nil || receipt.DispatchId != dispatch.DispatchID || receipt.OperationRef != dispatch.OperationID || receipt.Fence != dispatch.Fence || receipt.EnvironmentUuid != dispatch.EnvironmentUUID || receipt.ProjectUuid != dispatch.ProjectUUID || receipt.RequestDigest != dispatch.RequestDigest {
			return nil, invalidPluginDeploymentReceipt()
		}
		evidence.DeploymentID, evidence.Status = receipt.DeploymentId, string(receipt.Status)
		settle = slices.Contains([]string{"succeeded", "failed", "cancelled", "terminated"}, string(receipt.Status))
	}
	if settle {
		if err := model.CompleteExternalDispatch(ctx, s.Database, request.OrgId, item.ID, request.OperationId, request.DispatchId, evidence); err != nil {
			return nil, pluginModelError(err)
		}
	}
	return ReconcilePluginDeployment200JSONResponse{Status: result.Status, Body: result.Body}, nil
}
