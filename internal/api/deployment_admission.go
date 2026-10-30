package api

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"

	"github.com/google/uuid"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

// deploymentAdmissionCaller accepts only the Data Plane's admission identity,
// one for every organization, so an organization created later is admitted
// at once. IAM must vouch on every call that it is a platform principal with
// a static token: the Data Plane holds one token for every organization, and
// a service user of one organization would carry that organization's roles
// and stop serving every organization when that one is removed.
func (s *Server) deploymentAdmissionCaller(ctx context.Context, org string) error {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return err
	}
	if s.DeploymentAdmissionIdentity == uuid.Nil || actor != s.DeploymentAdmissionIdentity {
		return pluginBoundaryError(403, "deployment_admission_service_forbidden", "The Data Plane's admission identity is required.")
	}
	if s.PluginGateway != nil && s.PluginGateway.Registry != nil {
		for _, descriptor := range s.PluginGateway.Registry.List(ctx, org) {
			item, _, _ := s.PluginGateway.Registry.Installation(ctx, org, descriptor.ID)
			if item.ServiceUserID == actor.String() {
				return pluginBoundaryError(403, "deployment_admission_service_forbidden", "A plugin service cannot act as the Data Plane admission authority.")
			}
		}
	}
	static, err := s.staticPlatformPrincipal(ctx, actor)
	if err != nil {
		return pluginBoundaryError(503, "authorization_unavailable", "The Data Plane identity could not be verified. Retry after IAM recovers.")
	}
	if !static {
		return pluginBoundaryError(403, "deployment_admission_service_forbidden", "The Data Plane admission identity must be an IAM platform principal with a static token.")
	}
	middleware.SetAuthCheckedCtx(ctx)
	return nil
}

func deploymentAdmissionModelError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return pluginBoundaryError(404, "deployment_admission_not_found", "The admission does not exist in this scope.")
	}
	if _, ok := model.IsErrNotFound(err); ok {
		return pluginBoundaryError(404, "deployment_admission_not_found", "An admission resource does not exist in this scope.")
	}
	if _, ok := model.IsErrBadRequest(err); ok {
		return pluginBoundaryError(400, "deployment_admission_invalid", err.Error())
	}
	if conflict, ok := model.IsErrConflict(err); ok {
		code := conflict.Code
		if code == "" {
			code = "deployment_admission_conflict"
		}
		return pluginBoundaryError(409, code, err.Error())
	}
	return err
}

func deploymentAdmissionToAPI(receipt *model.DeploymentAdmissionReceipt) DeploymentAdmissionReceipt {
	return DeploymentAdmissionReceipt{AdmissionId: receipt.AdmissionID, ProjectUuid: receipt.ProjectUUID, EnvironmentUuid: receipt.EnvironmentUUID, RequestDigest: receipt.RequestDigest, Epoch: receipt.Epoch, State: DeploymentAdmissionReceiptState(receipt.State), Mode: receipt.Mode, Outcome: nonEmptyString(receipt.Outcome)}
}

func deploymentAdmissionFromAPI(org string, admission uuid.UUID, body *DeploymentAdmissionRequest) model.DeploymentAdmission {
	value := model.DeploymentAdmission{
		OrgID: org, AdmissionID: admission, ProjectUUID: body.ProjectUuid, EnvironmentUUID: body.EnvironmentUuid,
		ActorID: body.ActorId, RequestDigest: body.RequestDigest, Mode: string(body.Mode), ModuleVersions: body.ModuleVersions,
	}
	if body.DraftTestSnapshots != nil {
		value.DraftTestSnapshots = *body.DraftTestSnapshots
	}
	if body.CarryForwardModuleVersions != nil {
		value.CarryForwardModuleVersions = *body.CarryForwardModuleVersions
	}
	if body.ConfirmedRestrictedVersionUuids != nil {
		value.ConfirmedRestrictedVersionUUIDs = *body.ConfirmedRestrictedVersionUuids
	}
	return value
}

func (s *Server) deploymentAdmissionAuthorizationChecks(ctx context.Context, org string, body *DeploymentAdmissionRequest) ([]iamapi.ResourcePermissionCheck, bool, error) {
	checks := []iamapi.ResourcePermissionCheck{environmentCheck(body.EnvironmentUuid, "deployment_write")}
	carryForward := map[string]uuid.UUID{}
	if body.CarryForwardModuleVersions != nil {
		carryForward = *body.CarryForwardModuleVersions
	}
	required := make(map[string]struct{})
	requiresFreshProposedDecision := false
	if body.ConfirmedRestrictedVersionUuids != nil && len(*body.ConfirmedRestrictedVersionUuids) > 0 {
		permission := PermissionModuleVersionPinDefective
		if body.Mode == AdmissionModeRollback {
			permission = PermissionModuleRollbackRestricted
		}
		required[permission] = struct{}{}
	}
	for _, module := range slices.Sorted(maps.Keys(body.ModuleVersions)) {
		versionUUID := body.ModuleVersions[module]
		if body.DraftTestSnapshots != nil {
			if snapshotUUID, selected := (*body.DraftTestSnapshots)[module]; selected {
				if snapshotUUID != versionUUID {
					return nil, false, model.NewErrBadRequest("Draft test snapshots must match resolved Module targets")
				}
				if _, err := s.Database.GetModuleDraftTestSnapshot(ctx, nil, org, module, body.EnvironmentUuid, snapshotUUID); err != nil {
					return nil, false, err
				}
				if body.Mode != AdmissionModeRollback && carryForward[module] != snapshotUUID {
					if err := s.checkOrgAuthorization(ctx, body.ActorId, org, PermissionModuleVersionPublish); err != nil {
						return nil, false, err
					}
				}
				continue
			}
		}
		version, err := s.Database.GetCoreModuleVersion(ctx, nil, org, module, versionUUID.String(), model.GetModeDefault)
		if err != nil {
			return nil, false, err
		}
		switch version.LifecycleStatus {
		case moduleversions.LifecycleProposed:
			if body.Mode != AdmissionModeRollback && carryForward[module] != versionUUID {
				requiresFreshProposedDecision = true
			}
		}
	}
	for _, permission := range slices.Sorted(maps.Keys(required)) {
		checks = append(checks, environmentCheck(body.EnvironmentUuid, permission))
	}
	return checks, requiresFreshProposedDecision, nil
}

func (s *Server) AuthorizeDeploymentAdmission(ctx context.Context, request AuthorizeDeploymentAdmissionRequestObject) (AuthorizeDeploymentAdmissionResponseObject, error) {
	if err := s.deploymentAdmissionCaller(ctx, request.OrgId); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "deployment_admission_invalid", "An exact resolved Deployment admission is required.")
	}
	body := request.Body
	environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, request.OrgId, body.EnvironmentUuid, model.GetModeDefault)
	if err != nil {
		return nil, deploymentAdmissionModelError(err)
	}
	if environment.ProjectUuid != body.ProjectUuid {
		return nil, pluginBoundaryError(403, "deployment_admission_scope_forbidden", "The Environment does not belong to this Project and organization.")
	}
	if body.Mode == AdmissionModeDestroy {
		// Deletion is an already-authorized Core lifecycle workflow. Its DP
		// event currently carries the internal actor, not the original human.
		// No other action can use this exception, and the model rechecks the
		// deleting state and pending overrides inside the guard transaction.
		if environment.Status != model.EnvironmentStatusDeleting {
			return nil, pluginBoundaryError(409, "deployment_admission_conflict", "Destroy requires an Environment with confirmed deletion in progress.")
		}
	} else {
		checks, requiresFreshProposedDecision, err := s.deploymentAdmissionAuthorizationChecks(ctx, request.OrgId, body)
		if err != nil {
			return nil, deploymentAdmissionModelError(err)
		}
		if err := s.pluginAuthorize(ctx, body.ActorId, checks); err != nil {
			return nil, err
		}
		allowFreshProposed := false
		if requiresFreshProposedDecision {
			allowFreshProposed, err = s.pluginAuthorizationDecision(ctx, body.ActorId, []iamapi.ResourcePermissionCheck{
				environmentCheck(body.EnvironmentUuid, PermissionModuleVersionUseProposed),
			})
			if err != nil {
				return nil, err
			}
		}
		receipt, err := model.AuthorizeDeploymentAdmission(ctx, s.Database, deploymentAdmissionFromAPI(request.OrgId, request.AdmissionId, body), model.DeploymentAdmissionPolicy{AllowFreshProposed: allowFreshProposed, AllowFreshDraftTests: true})
		if err != nil {
			return nil, deploymentAdmissionModelError(err)
		}
		return AuthorizeDeploymentAdmission200JSONResponse(deploymentAdmissionToAPI(receipt)), nil
	}
	receipt, err := model.AuthorizeDeploymentAdmission(ctx, s.Database, deploymentAdmissionFromAPI(request.OrgId, request.AdmissionId, body), model.DeploymentAdmissionPolicy{})
	if err != nil {
		return nil, deploymentAdmissionModelError(err)
	}
	return AuthorizeDeploymentAdmission200JSONResponse(deploymentAdmissionToAPI(receipt)), nil
}

func (s *Server) SettleDeploymentAdmission(ctx context.Context, request SettleDeploymentAdmissionRequestObject) (SettleDeploymentAdmissionResponseObject, error) {
	if err := s.deploymentAdmissionCaller(ctx, request.OrgId); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "deployment_admission_invalid", "An exact durable Data Plane outcome is required.")
	}
	body := request.Body
	receipt, err := model.SettleDeploymentAdmission(ctx, s.Database, request.OrgId, request.AdmissionId, body.ProjectUuid, body.EnvironmentUuid, body.RequestDigest, string(body.Outcome))
	if err != nil {
		return nil, deploymentAdmissionModelError(err)
	}
	return SettleDeploymentAdmission200JSONResponse(deploymentAdmissionToAPI(receipt)), nil
}

func (s *Server) SealDeploymentAdmissionAbsence(ctx context.Context, request SealDeploymentAdmissionAbsenceRequestObject) (SealDeploymentAdmissionAbsenceResponseObject, error) {
	if err := s.deploymentAdmissionCaller(ctx, request.OrgId); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "deployment_admission_invalid", "The exact durable absent Data Plane intent is required.")
	}
	body := request.Body
	receipt, err := model.SealDeploymentAdmissionAbsence(ctx, s.Database, deploymentAdmissionFromAPI(request.OrgId, request.AdmissionId, body))
	if err != nil {
		return nil, deploymentAdmissionModelError(err)
	}
	return SealDeploymentAdmissionAbsence200JSONResponse(deploymentAdmissionToAPI(receipt)), nil
}
