package api

import (
	"context"
	"encoding/json"
	"errors"

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
	item, _, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
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
		if body.ModuleVersions != nil || body.DeploymentId != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Readiness accepts only Environment references.")
		}
	case PluginDeploymentReadRequestKindPlan:
		action = "external-deployment.plan"
		if len(body.Environments) != 1 || body.ModuleVersions == nil || len(*body.ModuleVersions) == 0 || len(*body.ModuleVersions) > 1000 || body.DeploymentId != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Planning requires one Environment and exact Module Versions.")
		}
	case PluginDeploymentReadRequestKindDeployment:
		action = "external-deployment.read"
		if len(body.Environments) != 1 || body.DeploymentId == nil || *body.DeploymentId == uuid.Nil || body.ModuleVersions != nil {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Deployment inspection requires one Environment and Deployment identity.")
		}
	default:
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Unsupported Deployment inspection kind.")
	}
	policy, ok := item.ServiceGrant(action)
	if !ok {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation has no grant for this Data Plane service port.")
	}
	claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, action, pluginProofFromAPI(body.Proof))
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated delegated actor is required.")
	}
	environments := make([]pluginDeploymentEnvironment, 0, len(body.Environments))
	checks := []orchestratoriam.ResourcePermissionCheck{}
	seen := map[uuid.UUID]bool{}
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
		checks = append(checks, environmentCheck(environment.Uuid, policy.Permission))
	}
	if policy.Scope == pluginOrganizationScope {
		checks = []orchestratoriam.ResourcePermissionCheck{orgCheck(request.OrgId, policy.Permission)}
	}
	if err := s.pluginAuthorize(ctx, actor, checks); err != nil {
		return nil, err
	}
	downstream := plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: actor, Action: action, RequestID: claims.RequestID}
	switch body.Kind {
	case PluginDeploymentReadRequestKindReadiness:
		downstream.Body, err = json.Marshal(struct {
			Environments []pluginDeploymentEnvironment `json:"environments"`
		}{environments})
	case PluginDeploymentReadRequestKindPlan:
		versions := map[string]pluginDeploymentModule{}
		for moduleRef, versionID := range *body.ModuleVersions {
			version, err := s.Database.GetCoreModuleVersion(ctx, nil, request.OrgId, moduleRef, versionID.String(), model.GetModeDefault)
			if err != nil {
				return nil, pluginModelError(err)
			}
			if _, duplicate := versions[version.ModuleSlug]; duplicate || version.SemanticVersion == nil {
				return nil, pluginBoundaryError(400, "plugin_request_invalid", "Plan targets require distinct Modules and immutable semantic versions.")
			}
			versions[version.ModuleSlug] = pluginDeploymentModule{ModuleUUID: version.ModuleUUID, Version: version.OpaqueVersionID, VersionUUID: version.UUID, SemanticVersion: *version.SemanticVersion, ArtifactDigest: version.ArtifactDigest}
		}
		downstream.Body, err = json.Marshal(struct {
			pluginDeploymentEnvironment
			ModuleVersions map[string]pluginDeploymentModule `json:"module_versions"`
		}{environments[0], versions})
	case PluginDeploymentReadRequestKindDeployment:
		downstream.ProjectUUID = environments[0].ProjectUUID
		downstream.EnvironmentUUID = environments[0].EnvironmentUUID
		downstream.DeploymentID = *body.DeploymentId
	}
	if err != nil {
		return nil, err
	}
	result, err := s.PluginDataPlane.Call(ctx, downstream)
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	return ReadPluginDeploymentData200JSONResponse{Status: result.Status, Body: result.Body}, nil
}

func pluginGatewayError(err error) error {
	var gatewayError *plugins.GatewayError
	if errors.As(err, &gatewayError) {
		return pluginBoundaryError(gatewayError.Status, gatewayError.Code, gatewayError.Message)
	}
	return err
}
