package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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
	action := pluginDeploymentSubmitAction(*body)
	grant, environment, err := s.authorizePluginEnvironmentOperation(ctx, request.OrgId, item, descriptor, request.OperationId, body.GrantFence, body.EnvironmentUuid, action, body.Mode == PluginDeploymentSubmitModeRollback)
	if err != nil {
		return nil, err
	}
	if grant.State != "active" {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Deployment admission requires an active operation.")
	}
	dispatch, encoded, upstream, err := s.pluginDeploymentIntent(ctx, request.OrgId, item, grant, environment, *body, nil, false)
	if err != nil {
		return nil, err
	}
	warning := pluginServiceWarning(item, descriptor, action)
	if upstream != nil {
		return SubmitPluginDeployment200JSONResponse{Status: upstream.Status, Body: upstream.Body, EntitlementWarning: warning}, nil
	}
	if err := s.authorizePluginDispatchApproval(ctx, item, dispatch); err != nil {
		return nil, err
	}
	if err := model.RecordExternalDispatch(ctx, s.pluginEffectDatabase(request.OrgId, item, action, body.Mode == PluginDeploymentSubmitModeRollback), dispatch); err != nil {
		return nil, pluginModelError(err)
	}
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: item.ID, ActorID: grant.ActorID, Action: "external-deployment.admit", RequestID: body.RequestId, Body: encoded})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	return SubmitPluginDeployment200JSONResponse{Status: result.Status, Body: result.Body, EntitlementWarning: warning}, nil
}

func pluginDeploymentSubmitAction(body PluginDeploymentSubmitRequest) string {
	if body.Mode == PluginDeploymentSubmitModeRollback {
		return "external-deployment.rollback"
	}
	return "external-deployment.admit"
}

// The original fence belongs to the downstream bytes. Recovery authorization
// uses the current grant fence separately and never rewrites this identity.
func (s *Server) pluginDeploymentIntent(ctx context.Context, org string, item plugins.Installation, grant *model.ExternalOperationGrant, environment pluginDeploymentEnvironment, body PluginDeploymentSubmitRequest, retained *model.ExternalDispatch, withdraw bool) (model.ExternalDispatch, []byte, *plugins.InvocationResult, error) {
	empty := model.ExternalDispatch{}
	admission := dpapi.ExternalDeploymentAdmissionBody{OperationRef: grant.OperationID, Fence: body.GrantFence, ProjectId: environment.ProjectID, EnvId: environment.EnvironmentID, ProjectUuid: environment.ProjectUUID, EnvironmentUuid: environment.EnvironmentUUID, Mode: dpapi.ExternalDeploymentAdmissionBodyMode(body.Mode), FirstForwardDeploymentId: body.FirstForwardDeploymentId}
	targets := map[string]uuid.UUID{}
	if body.Plan != nil {
		admission.Plan = &dpapi.ExternalDeploymentAdmissionPlan{SnapshotId: body.Plan.SnapshotId, Fingerprint: body.Plan.Fingerprint, ExpectedModuleVersions: body.Plan.ModuleVersions}
		targets = body.Plan.ModuleVersions
	} else if retained != nil {
		// The receipt and target bindings were verified before recording this
		// immutable intent. Do not refresh them from mutable history on recovery.
		targets = retained.TargetVersions
		admission.Rollback = &dpapi.ExternalDeploymentAdmissionRollback{ForwardDeploymentId: body.Rollback.ForwardDeploymentId, RetryOfRollbackDeploymentId: body.Rollback.RetryOfRollbackDeploymentId}
	} else {
		// A plugin cannot choose an arbitrary historical rollback target. Only
		// DP's retained base of this operation's own forward effect is eligible.
		result, err := s.readPluginDeploymentReceipt(ctx, org, item.ID, grant.ActorID, environment, body.Rollback.ForwardDeploymentId, "", "base-"+plugincontext.Digest([]byte(body.RequestId)))
		if err != nil {
			return empty, nil, nil, err
		}
		if result.Status != http.StatusOK {
			return empty, nil, result, nil
		}
		var receipt dpapi.ExternalDeploymentStatusResponse
		if err := json.Unmarshal(result.Body, &receipt); err != nil {
			return empty, nil, nil, invalidPluginDeploymentReceipt()
		}
		if receipt.DeploymentId != body.Rollback.ForwardDeploymentId || receipt.OperationRef != grant.OperationID || receipt.EnvironmentUuid != environment.EnvironmentUUID || receipt.ProjectUuid != environment.ProjectUUID || receipt.Mode != dpapi.ExternalDeploymentStatusModeDeploy || !withdraw && (!slices.Contains([]dpapi.ExternalDeploymentStatusResponseStatus{dpapi.ExternalDeploymentStatusSucceeded, dpapi.ExternalDeploymentStatusFailed}, receipt.Status) || receipt.Status == dpapi.ExternalDeploymentStatusFailed && receipt.CompletedAt == nil) {
			return empty, nil, nil, pluginBoundaryError(409, "plugin_operation_conflict", "Rollback requires this operation's terminal successful or failed forward Deployment and exact captured base.")
		}
		if err := validatePluginRollbackAnchor(body.FirstForwardDeploymentId, receipt); err != nil {
			return empty, nil, nil, err
		}
		if body.Rollback.RetryOfRollbackDeploymentId != nil {
			upstream, err := s.validatePluginRollbackRetry(ctx, org, item, grant, environment, body, receipt)
			if err != nil || upstream != nil {
				return empty, nil, upstream, err
			}
		}
		for module, version := range receipt.RollbackModuleVersions {
			if _, owned := grant.Intent.TargetVersions[module]; owned {
				targets[module] = version
			}
		}
		if len(targets) == 0 {
			return empty, nil, nil, invalidPluginDeploymentReceipt()
		}
		admission.Rollback = &dpapi.ExternalDeploymentAdmissionRollback{ForwardDeploymentId: receipt.DeploymentId, RetryOfRollbackDeploymentId: body.Rollback.RetryOfRollbackDeploymentId}
	}
	encoded, err := json.Marshal(admission)
	if err != nil {
		return empty, nil, nil, err
	}
	dispatch := model.ExternalDispatch{OrgID: org, PluginID: item.ID, OperationID: grant.OperationID, Fence: body.GrantFence, ActorID: grant.ActorID, Action: pluginDeploymentSubmitAction(body), DispatchID: body.RequestId, ProjectUUID: environment.ProjectUUID, EnvironmentUUID: environment.EnvironmentUUID, RequestDigest: plugincontext.Digest(encoded), TargetVersions: targets}
	if retained != nil {
		// Safety activation can rotate the current operator. Recovery retains
		// the recorded effect's original actor; IAM checks the current operator.
		dispatch.ActorID = retained.ActorID
	}
	if body.ApprovedPinBoundaries != nil {
		dispatch.ApprovedPins = *body.ApprovedPinBoundaries
	}
	if body.Approval != nil {
		dispatch.Approval = &model.ExternalDispatchApproval{ActorID: body.Approval.ActorId, Scope: string(body.Approval.Scope)}
	}
	return dispatch, encoded, nil, nil
}

func validatePluginDeploymentSubmit(body PluginDeploymentSubmitRequest) error {
	invalid := pluginBoundaryError(400, "plugin_request_invalid", "Supply one exact planned deployment or captured-base rollback, with matching approval evidence.")
	if body.GrantFence <= 0 || body.EnvironmentUuid == uuid.Nil || body.RequestId == "" || len(body.RequestId) > 128 || (body.Approval == nil) != (body.ApprovedPinBoundaries == nil) {
		return invalid
	}
	if body.FirstForwardDeploymentId != nil && *body.FirstForwardDeploymentId == uuid.Nil {
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
		if retry := body.Rollback.RetryOfRollbackDeploymentId; retry != nil && (*retry == uuid.Nil || *retry == body.Rollback.ForwardDeploymentId) {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

// Legacy first-attempt receipts may omit both fields. New lineage assertions
// and explicit failed-rollback retries require the authoritative DP anchor.
func validatePluginRollbackAnchor(expected *uuid.UUID, forward dpapi.ExternalDeploymentStatusResponse) error {
	first, base := forward.FirstForwardDeploymentId, forward.RollbackBaseDeploymentId
	if (first == nil) != (base == nil) || first != nil && (*first == uuid.Nil || *base == uuid.Nil) {
		return invalidPluginDeploymentReceipt()
	}
	if expected != nil && (first == nil || *first != *expected) {
		return pluginBoundaryError(409, "plugin_operation_conflict", "The first forward Deployment does not match this operation's immutable rollback lineage.")
	}
	return nil
}

func (s *Server) validatePluginRollbackRetry(ctx context.Context, org string, item plugins.Installation, grant *model.ExternalOperationGrant, environment pluginDeploymentEnvironment, body PluginDeploymentSubmitRequest, forward dpapi.ExternalDeploymentStatusResponse) (*plugins.InvocationResult, error) {
	if forward.FirstForwardDeploymentId == nil || forward.RollbackBaseDeploymentId == nil {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Rollback retry requires retained immutable deployment lineage.")
	}
	predecessor := *body.Rollback.RetryOfRollbackDeploymentId
	result, err := s.readPluginDeploymentReceipt(ctx, org, item.ID, grant.ActorID, environment, predecessor, "", "retry-"+plugincontext.Digest([]byte(body.RequestId)))
	if err != nil {
		return nil, err
	}
	if result.Status != http.StatusOK {
		return result, nil
	}
	var receipt dpapi.ExternalDeploymentStatusResponse
	if err := json.Unmarshal(result.Body, &receipt); err != nil {
		return nil, invalidPluginDeploymentReceipt()
	}
	admissionBase := receipt.ForwardDeploymentId
	if receipt.RetryOfRollbackDeploymentId != nil {
		admissionBase = receipt.RetryOfRollbackDeploymentId
	}
	if receipt.DeploymentId != predecessor || receipt.OperationRef != grant.OperationID || receipt.ProjectUuid != environment.ProjectUUID || receipt.EnvironmentUuid != environment.EnvironmentUUID ||
		receipt.Mode != dpapi.ExternalDeploymentStatusModeRollback || receipt.Status != dpapi.ExternalDeploymentStatusFailed || receipt.CompletedAt == nil ||
		receipt.ForwardDeploymentId == nil || *receipt.ForwardDeploymentId != body.Rollback.ForwardDeploymentId ||
		receipt.FirstForwardDeploymentId == nil || *receipt.FirstForwardDeploymentId != *forward.FirstForwardDeploymentId ||
		receipt.RollbackBaseDeploymentId == nil || *receipt.RollbackBaseDeploymentId != *forward.RollbackBaseDeploymentId ||
		admissionBase == nil || *admissionBase == uuid.Nil || *admissionBase == predecessor ||
		receipt.AdmissionBaseDeploymentId == nil || *receipt.AdmissionBaseDeploymentId != *admissionBase {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Explicit rollback retry requires this operation's failed rollback of the same Environment to the original captured base.")
	}
	// DP rechecks current head, authoritative Runner completion, base and lease
	// atomically. This preliminary read never grants infrastructure authority.
	return nil, nil
}

func (s *Server) authorizePluginEnvironmentOperation(ctx context.Context, org string, item plugins.Installation, descriptor plugins.Descriptor, operation uuid.UUID, fence int64, environmentID uuid.UUID, action string, safety bool) (*model.ExternalOperationGrant, pluginDeploymentEnvironment, error) {
	empty := pluginDeploymentEnvironment{}
	policy, ok := item.ServiceGrant(action)
	if !ok {
		return nil, empty, pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not declare this service action.")
	}
	if err := pluginFeatureAllowed(descriptor, policy.RequiredFeature, true, safety); err != nil {
		return nil, empty, err
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
	allowed, err := s.pluginAuthorizationDecision(ctx, dispatch.Approval.ActorID, []orchestratoriam.ResourcePermissionCheck{check})
	if err != nil {
		return err
	}
	if !allowed {
		return pluginBoundaryError(403, "plugin_approval_forbidden", "The original approver no longer has the required capability and scope.")
	}
	return nil
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
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing || missing && request.Body.OriginalRequest == nil {
		return nil, pluginModelError(err)
	}
	environmentID := uuid.Nil
	if dispatch != nil {
		environmentID = dispatch.EnvironmentUUID
	}
	if original := request.Body.OriginalRequest; original != nil {
		if err := validatePluginDeploymentSubmit(*original); err != nil {
			return nil, err
		}
		if original.RequestId != request.DispatchId || original.GrantFence > request.Body.GrantFence || dispatch != nil && original.EnvironmentUuid != dispatch.EnvironmentUUID {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Recovery must preserve the original dispatch identity, Environment and fence.")
		}
		environmentID = original.EnvironmentUuid
	}
	action := pluginDeploymentReadAction
	if request.Body.Withdraw {
		action = "external-deployment.withdraw"
	}
	grant, environment, err := s.authorizePluginEnvironmentOperation(ctx, request.OrgId, item, descriptor, request.OperationId, request.Body.GrantFence, environmentID, action, true)
	if err != nil {
		return nil, err
	}
	if original := request.Body.OriginalRequest; original != nil {
		originalAction := pluginDeploymentSubmitAction(*original)
		if !slices.Contains(grant.Intent.Actions, originalAction) {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The original action is outside the frozen operation grant.")
		}
		if _, ok := item.ServiceGrant(originalAction); !ok {
			return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not declare the original service action.")
		}
		recovered, _, upstream, err := s.pluginDeploymentIntent(ctx, request.OrgId, item, grant, environment, *original, dispatch, request.Body.Withdraw)
		if err != nil {
			return nil, err
		}
		if upstream != nil {
			// A missing base receipt is not evidence that the dispatch had no
			// effect. No exact Core barrier or dispatch lookup exists yet.
			return nil, invalidPluginDeploymentReceipt()
		}
		if dispatch != nil && !reflect.DeepEqual(*dispatch, recovered) {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The original request differs from Core's immutable dispatch intent.")
		}
		dispatch = &recovered
		if missing && !request.Body.Withdraw {
			// Recreate authority, not work. The subsequent DP lookup serializes
			// with any original admission before the client may replay Submit.
			if _, _, err := s.authorizePluginEnvironmentOperation(ctx, request.OrgId, item, descriptor, request.OperationId, original.GrantFence, environmentID, originalAction, original.Mode == PluginDeploymentSubmitModeRollback); err != nil {
				return nil, err
			}
			if err := s.authorizePluginDispatchApproval(ctx, item, recovered); err != nil {
				return nil, err
			}
			if err := model.RecordExternalDispatch(ctx, s.pluginEffectDatabase(request.OrgId, item, originalAction, original.Mode == PluginDeploymentSubmitModeRollback), recovered); err != nil {
				return nil, pluginModelError(err)
			}
		}
	}
	if request.Body.Withdraw {
		// Commit withdrawal before DP receipt acquisition. Holding a Core lock
		// across the HTTP request would deadlock DP's admission callback.
		if err := model.RecoverWithdrawnExternalDispatch(ctx, s.pluginEffectDatabase(request.OrgId, item, action, true), *dispatch, request.Body.GrantFence); err != nil {
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
		if err := model.CompleteExternalDispatch(ctx, s.pluginEffectDatabase(request.OrgId, item, action, true), request.OrgId, item.ID, request.OperationId, request.DispatchId, evidence); err != nil {
			return nil, pluginModelError(err)
		}
	}
	return ReconcilePluginDeployment200JSONResponse{Status: result.Status, Body: result.Body}, nil
}
