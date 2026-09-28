package api

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

const pluginAuthorizationCheckAction = "authorization.check"

type pluginAuthorizationResource struct {
	scope     string
	id        string
	projectID uuid.UUID
}

func (s *Server) CheckPluginAuthorization(ctx context.Context, request CheckPluginAuthorizationRequestObject) (CheckPluginAuthorizationResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact authorization check is required.")
	}
	body := request.Body
	proofAuthority := body.Proof != nil
	grantAuthority := body.OperationId != nil || body.GrantFence != nil
	if proofAuthority == grantAuthority || grantAuthority && (body.OperationId == nil || body.GrantFence == nil) || proofAuthority && body.SubjectId != nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Choose exactly one complete authorization authority.")
	}
	policy, ok := item.AuthorizationPolicy(body.Permission)
	if !ok {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation does not permit this authorization check.")
	}
	resources, checks, err := s.resolvePluginAuthorizationResources(ctx, request.OrgId, policy, body.Permission, body.Resources)
	if err != nil {
		return nil, err
	}

	var actor uuid.UUID
	if proofAuthority {
		claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, pluginAuthorizationCheckAction, pluginProofFromAPI(*body.Proof))
		if err != nil {
			var denied *plugins.GatewayError
			if errors.As(err, &denied) {
				return nil, pluginBoundaryError(denied.Status, denied.Code, denied.Message)
			}
			return nil, err
		}
		actor, err = uuid.Parse(claims.ActorID)
		if err != nil || actor == uuid.Nil {
			return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated delegated actor is required.")
		}
	} else {
		if descriptor.State != plugins.StateAvailable && descriptor.State != plugins.StateNotEnabled && descriptor.State != plugins.StateNotEntitled {
			return nil, pluginBoundaryError(409, "plugin_"+string(descriptor.State), "The plugin cannot check authorization in its current availability state.")
		}
		grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, *body.OperationId)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, pluginBoundaryError(404, "plugin_operation_not_found", "The operation grant does not exist in this scope.")
		}
		if err != nil {
			return nil, err
		}
		if grant.Revision != *body.GrantFence {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The operation grant fence is no longer current.")
		}
		allowed, err := s.pluginGrantAuthorizationDecision(ctx, request.OrgId, item, grant, resources, checks, body.SubjectId)
		if err != nil {
			return nil, err
		}
		return CheckPluginAuthorization200JSONResponse{Allowed: allowed}, nil
	}

	allowed, err := s.pluginAuthorizationDecision(ctx, actor, checks)
	if err != nil {
		return nil, err
	}
	return CheckPluginAuthorization200JSONResponse{Allowed: allowed}, nil
}

func (s *Server) pluginGrantAuthorizationDecision(ctx context.Context, org string, item plugins.Installation, grant *model.ExternalOperationGrant, resources []pluginAuthorizationResource, checks []orchestratoriam.ResourcePermissionCheck, subject *uuid.UUID) (bool, error) {
	if !resourcesWithinGrant(resources, grant.Intent) {
		return false, pluginBoundaryError(403, "plugin_service_forbidden", "The requested resources exceed the frozen operation scope.")
	}
	controllerChecks, err := s.pluginAuthorizationControllerChecks(ctx, org, item, grant)
	if err != nil {
		return false, err
	}
	allowed, err := s.pluginAuthorizationDecision(ctx, grant.ActorID, controllerChecks)
	if err != nil || !allowed {
		return allowed, err
	}
	actor := grant.ActorID
	if subject != nil {
		actor = *subject
	}
	return s.pluginAuthorizationDecision(ctx, actor, checks)
}

func (s *Server) resolvePluginAuthorizationResources(ctx context.Context, org string, policy plugins.AuthorizationPolicy, permission string, requested []struct {
	Id   string                                       `json:"id"`
	Type PluginAuthorizationCheckRequestResourcesType `json:"type"`
}) ([]pluginAuthorizationResource, []orchestratoriam.ResourcePermissionCheck, error) {
	if len(requested) == 0 {
		return nil, nil, pluginBoundaryError(400, "plugin_request_invalid", "At least one exact authorization resource is required.")
	}
	resources := make([]pluginAuthorizationResource, 0, len(requested))
	checks := make([]orchestratoriam.ResourcePermissionCheck, 0, len(requested))
	seen := map[string]bool{}
	for _, request := range requested {
		scope := string(request.Type)
		key := scope + ":" + request.Id
		if request.Id == "" || seen[key] || !slices.Contains(policy.Scopes, scope) {
			return nil, nil, pluginBoundaryError(400, "plugin_request_invalid", "Authorization resources must be unique and use configured scopes.")
		}
		seen[key] = true
		resource := pluginAuthorizationResource{scope: scope, id: request.Id}
		switch request.Type {
		case PluginAuthorizationCheckRequestResourcesTypeOrganization:
			if request.Id != org {
				return nil, nil, pluginBoundaryError(403, "plugin_service_forbidden", "Authorization resources must belong to the path organization.")
			}
			checks = append(checks, orgCheck(org, permission))
		case PluginAuthorizationCheckRequestResourcesTypeProject:
			id, err := uuid.Parse(request.Id)
			if err != nil || id == uuid.Nil {
				return nil, nil, pluginBoundaryError(400, "plugin_request_invalid", "Project authorization resources require immutable UUIDs.")
			}
			project, err := s.Database.GetProjectByUuid(ctx, nil, org, id, model.GetModeDefault)
			if err != nil {
				return nil, nil, pluginAuthorizationMembershipError(err)
			}
			resource.projectID = project.Uuid
			checks = append(checks, projectCheck(project.Uuid, permission))
		case PluginAuthorizationCheckRequestResourcesTypeEnvironment:
			id, err := uuid.Parse(request.Id)
			if err != nil || id == uuid.Nil {
				return nil, nil, pluginBoundaryError(400, "plugin_request_invalid", "Environment authorization resources require immutable UUIDs.")
			}
			environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, org, id, model.GetModeDefault)
			if err != nil {
				return nil, nil, pluginAuthorizationMembershipError(err)
			}
			resource.projectID = environment.ProjectUuid
			checks = append(checks, environmentCheck(environment.Uuid, permission))
		default:
			return nil, nil, pluginBoundaryError(400, "plugin_request_invalid", "Unsupported authorization resource scope.")
		}
		resources = append(resources, resource)
	}
	return resources, checks, nil
}

func pluginAuthorizationMembershipError(err error) error {
	if _, ok := model.IsErrNotFound(err); ok {
		return pluginBoundaryError(403, "plugin_service_forbidden", "Authorization resources must belong to the path organization.")
	}
	return err
}

func resourcesWithinGrant(resources []pluginAuthorizationResource, intent model.ExternalOperationIntent) bool {
	projects := map[uuid.UUID]bool{}
	environments := map[uuid.UUID]uuid.UUID{}
	for _, target := range intent.Environments {
		projects[target.ProjectID] = true
		environments[target.EnvironmentID] = target.ProjectID
	}
	for _, resource := range resources {
		switch resource.scope {
		case "organization":
			continue
		case "project":
			id, err := uuid.Parse(resource.id)
			if err != nil || !projects[id] {
				return false
			}
		case "environment":
			id, err := uuid.Parse(resource.id)
			if err != nil || environments[id] != resource.projectID {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) pluginAuthorizationControllerChecks(ctx context.Context, org string, item plugins.Installation, grant *model.ExternalOperationGrant) ([]orchestratoriam.ResourcePermissionCheck, error) {
	serviceGrant, ok := item.ServiceGrant(pluginAuthorizationCheckAction)
	policy, policyOK := item.AuthorizationPolicy(serviceGrant.Permission)
	if !ok || !policyOK || !slices.Contains(policy.Scopes, serviceGrant.Scope) {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation has no usable authorization check grant.")
	}
	if serviceGrant.Scope == pluginOrganizationScope {
		return []orchestratoriam.ResourcePermissionCheck{orgCheck(org, serviceGrant.Permission)}, nil
	}
	if serviceGrant.Scope != "environment" || len(grant.Intent.Environments) == 0 {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The authorization check grant cannot cover this operation.")
	}
	checks := make([]orchestratoriam.ResourcePermissionCheck, 0, len(grant.Intent.Environments))
	for _, target := range grant.Intent.Environments {
		environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, org, target.EnvironmentID, model.GetModeDefault)
		if err != nil {
			return nil, pluginAuthorizationMembershipError(err)
		}
		if environment.ProjectUuid != target.ProjectID {
			return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The operation's frozen environment scope is no longer valid.")
		}
		checks = append(checks, environmentCheck(environment.Uuid, serviceGrant.Permission))
	}
	return checks, nil
}
