package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

type pluginDeploymentEnvironment struct {
	ProjectID       string    `json:"project_id"`
	EnvironmentID   string    `json:"env_id"`
	ProjectUUID     uuid.UUID `json:"project_uuid"`
	EnvironmentUUID uuid.UUID `json:"environment_uuid"`
}

type pluginDeploymentModule struct {
	ModuleUUID      uuid.UUID `json:"module_uuid"`
	Version         string    `json:"version"`
	VersionUUID     uuid.UUID `json:"version_uuid"`
	SemanticVersion string    `json:"semantic_version"`
	ArtifactDigest  string    `json:"artifact_digest"`
}

func (s *Server) ReadPluginDeploymentData(ctx context.Context, request ReadPluginDeploymentDataRequestObject) (ReadPluginDeploymentDataResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil || len(request.Body.Environments) == 0 || len(request.Body.Environments) > 1000 {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "A bounded Environment inspection request is required.")
	}
	body := request.Body
	action := ""
	switch body.Kind {
	case PluginDeploymentReadRequestKindReadiness:
		action = "external-deployment.readiness"
		if body.ModuleVersions != nil || body.DeploymentId != nil || body.DispatchId != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Readiness accepts only Environment references.")
		}
	case PluginDeploymentReadRequestKindPlan:
		action = "external-deployment.plan"
		if len(body.Environments) != 1 || body.ModuleVersions == nil || len(*body.ModuleVersions) == 0 || len(*body.ModuleVersions) > 1000 || body.DeploymentId != nil || body.DispatchId != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Planning requires one Environment and exact Module Versions.")
		}
	case PluginDeploymentReadRequestKindDeployment:
		action = pluginDeploymentReadAction
		if len(body.Environments) != 1 || body.DeploymentId == nil || *body.DeploymentId == uuid.Nil || body.ModuleVersions != nil || body.DispatchId != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Deployment inspection requires one Environment and Deployment identity.")
		}
	case PluginDeploymentReadRequestKindDispatch:
		action = pluginDeploymentReadAction
		if len(body.Environments) != 1 || body.DispatchId == nil || *body.DispatchId == "" || body.DeploymentId != nil || body.ModuleVersions != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Dispatch inspection requires one Environment and dispatch identity.")
		}
	default:
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Unsupported Deployment inspection kind.")
	}
	policy, ok := item.ServiceGrant(action)
	if !ok {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation has no grant for this Data Plane service port.")
	}
	actor, requestID, grant, err := s.pluginInspectionAuthority(ctx, request.OrgId, item, descriptor, action, body)
	if err != nil {
		return nil, err
	}
	environments := make([]pluginDeploymentEnvironment, 0, len(body.Environments))
	checks := []orchestratoriam.ResourcePermissionCheck{}
	seen := map[uuid.UUID]bool{}
	bounds := []model.ExternalOperationEnvironment{}
	for _, ref := range body.Environments {
		environment, err := s.Database.GetEnvironment(ctx, nil, request.OrgId, ref.ProjectId, ref.EnvId, model.GetModeDefault)
		if err != nil {
			return nil, pluginModelError(err)
		}
		if seen[environment.Uuid] {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Environment references must be unique.")
		}
		seen[environment.Uuid] = true
		environments = append(environments, pluginDeploymentEnvironment{ProjectID: environment.ProjectId, EnvironmentID: environment.Id, ProjectUUID: environment.ProjectUuid, EnvironmentUUID: environment.Uuid})
		bounds = append(bounds, model.ExternalOperationEnvironment{ProjectID: environment.ProjectUuid, EnvironmentID: environment.Uuid})
		checks = append(checks, environmentCheck(environment.Uuid, policy.Permission))
	}
	if policy.Scope == pluginOrganizationScope {
		checks = []orchestratoriam.ResourcePermissionCheck{orgCheck(request.OrgId, policy.Permission)}
	}
	if err := s.pluginAuthorize(ctx, actor, checks); err != nil {
		return nil, err
	}
	if grant != nil {
		if err := model.ValidateExternalOperationRead(grant, *body.GrantFence, action, bounds, nil); err != nil {
			return nil, pluginModelError(err)
		}
	}
	downstream := plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: actor, Action: action}
	switch body.Kind {
	case PluginDeploymentReadRequestKindReadiness:
		downstream.Body, err = json.Marshal(struct {
			Environments []pluginDeploymentEnvironment `json:"environments"`
		}{environments})
	case PluginDeploymentReadRequestKindPlan:
		versions := map[string]pluginDeploymentModule{}
		versionBounds := map[uuid.UUID]uuid.UUID{}
		for moduleRef, versionID := range *body.ModuleVersions {
			version, err := s.Database.GetCoreModuleVersion(ctx, nil, request.OrgId, moduleRef, versionID.String(), model.GetModeDefault)
			if err != nil {
				return nil, pluginModelError(err)
			}
			if _, duplicate := versions[version.ModuleSlug]; duplicate || version.SemanticVersion == nil {
				return nil, pluginBoundaryError(400, "plugin_request_invalid", "Plan targets require distinct Modules and immutable semantic versions.")
			}
			versions[version.ModuleSlug] = pluginDeploymentModule{ModuleUUID: version.ModuleUUID, Version: version.OpaqueVersionID, VersionUUID: version.UUID, SemanticVersion: *version.SemanticVersion, ArtifactDigest: version.ArtifactDigest}
			versionBounds[version.ModuleUUID] = version.UUID
		}
		if grant != nil {
			if err := model.ValidateExternalOperationRead(grant, *body.GrantFence, action, bounds, versionBounds); err != nil {
				return nil, pluginModelError(err)
			}
		}
		downstream.Body, err = json.Marshal(struct {
			pluginDeploymentEnvironment
			ModuleVersions map[string]pluginDeploymentModule `json:"module_versions"`
		}{environments[0], versions})
	case PluginDeploymentReadRequestKindDeployment, PluginDeploymentReadRequestKindDispatch:
		downstream.ProjectUUID = environments[0].ProjectUUID
		downstream.EnvironmentUUID = environments[0].EnvironmentUUID
		if body.DeploymentId != nil {
			downstream.DeploymentID = *body.DeploymentId
		} else {
			downstream.DispatchID = *body.DispatchId
		}
	}
	if err != nil {
		return nil, err
	}
	// A single preview fans out to many Environments. Each exact downstream
	// request needs its own stable identity, not the shared incoming command ID.
	identity, err := json.Marshal(struct {
		Source  string
		Request plugins.DataPlaneRequest
	}{requestID, downstream})
	if err != nil {
		return nil, err
	}
	downstream.RequestID = fmt.Sprintf("inspection-%x", sha256.Sum256(identity))
	result, err := s.PluginDataPlane.Call(ctx, downstream)
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	return ReadPluginDeploymentData200JSONResponse{Status: result.Status, Body: result.Body}, nil
}

func (s *Server) pluginInspectionAuthority(ctx context.Context, org string, item plugins.Installation, descriptor plugins.Descriptor, action string, body *PluginDeploymentReadRequest) (uuid.UUID, string, *model.ExternalOperationGrant, error) {
	invalid := pluginBoundaryError(400, "plugin_request_invalid", "Supply either one fresh user proof or one operation ID, current fence and inspection request ID.")
	if body.Proof != nil {
		if body.OperationId != nil || body.GrantFence != nil || body.RequestId != nil {
			return uuid.Nil, "", nil, invalid
		}
		claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, org, item.ID, item.ServiceUserID, action, pluginProofFromAPI(*body.Proof))
		if err != nil {
			return uuid.Nil, "", nil, pluginGatewayError(err)
		}
		actor, err := uuid.Parse(claims.ActorID)
		if err != nil {
			return uuid.Nil, "", nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated delegated actor is required.")
		}
		return actor, claims.RequestID, nil, nil
	}
	if body.OperationId == nil || *body.OperationId == uuid.Nil || body.GrantFence == nil || *body.GrantFence <= 0 || body.RequestId == nil || *body.RequestId == "" || len(*body.RequestId) > 128 || strings.ContainsAny(*body.RequestId, "\r\n") {
		return uuid.Nil, "", nil, invalid
	}
	if err := pluginServiceAllowed(item, descriptor, action, false, false); err != nil {
		return uuid.Nil, "", nil, err
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, org, item.ID, *body.OperationId)
	if err != nil {
		return uuid.Nil, "", nil, pluginModelError(err)
	}
	return grant.ActorID, *body.RequestId, grant, nil
}

func pluginGatewayError(err error) error {
	var gatewayError *plugins.GatewayError
	if errors.As(err, &gatewayError) {
		return pluginBoundaryError(gatewayError.Status, gatewayError.Code, gatewayError.Message)
	}
	return err
}
