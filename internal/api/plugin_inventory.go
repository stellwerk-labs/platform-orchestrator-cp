package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func (s *Server) ListProposedModuleVersions(ctx context.Context, request ListProposedModuleVersionsRequestObject) (ListProposedModuleVersionsResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	items, err := model.ListProposedCoreModuleVersions(ctx, s.Database, request.OrgId)
	if err != nil {
		return nil, err
	}
	response := ListProposedModuleVersions200JSONResponse{}
	for _, item := range items {
		response = append(response, ModuleVersionLifecycle{ModuleId: item.ModuleSlug, ModuleUuid: item.ModuleUUID, Version: item.OpaqueVersionID, VersionUuid: item.UUID, ArtifactDigest: item.ArtifactDigest, Status: ModuleVersionSemanticStatus(item.LifecycleStatus), CreatedAt: item.CreatedAt})
	}
	return response, nil
}

func (s *Server) ReadPluginInventory(ctx context.Context, request ReadPluginInventoryRequestObject) (ReadPluginInventoryResponseObject, error) {
	item, _, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An inventory request is required.")
	}
	policy, ok := item.ServiceGrant("inventory.read")
	if !ok {
		return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The installation has no inventory read grant.")
	}
	claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, "inventory.read", pluginProofFromAPI(request.Body.Proof))
	if err != nil {
		var denied *plugins.GatewayError
		if errors.As(err, &denied) {
			return nil, pluginBoundaryError(denied.Status, denied.Code, denied.Message)
		}
		return nil, err
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated actor is required.")
	}
	selector := request.Body.Selector
	snapshot, err := model.ReadExternalInventory(ctx, s.Database, request.OrgId, request.Body.ModuleIds, model.ExternalInventorySelector{EnvironmentIDs: ref.DerefOr(selector.EnvironmentIds, nil), ProjectIDs: ref.DerefOr(selector.ProjectIds, nil), EnvironmentTypes: ref.DerefOr(selector.EnvironmentTypes, nil), Labels: ref.DerefOr(selector.Labels, nil)})
	if err != nil {
		return nil, pluginModelError(err)
	}
	checks := []orchestratoriam.ResourcePermissionCheck{}
	if policy.Scope == pluginOrganizationScope || len(snapshot.Environments) == 0 {
		checks = append(checks, orgCheck(request.OrgId, policy.Permission))
	} else {
		for _, environment := range snapshot.Environments {
			checks = append(checks, environmentCheck(environment.Uuid, policy.Permission))
		}
	}
	if err := s.pluginAuthorize(ctx, actor, checks); err != nil {
		return nil, err
	}
	return ReadPluginInventory200JSONResponse(externalInventoryToAPI(snapshot)), nil
}

func externalInventoryToAPI(snapshot *model.ExternalInventory) PluginInventory {
	result := PluginInventory{Environments: []Environment{}, EnvironmentTypes: []EnvironmentType{}, Modules: []ModuleCatalogueEntry{}, Versions: []CoreModuleVersion{}, Pins: []EnvironmentModuleVersionPin{}, PinEvents: []ModuleVersionPinEvent{}}
	for _, environment := range snapshot.Environments {
		result.Environments = append(result.Environments, envFromDbModel(&environment))
	}
	for _, envType := range snapshot.EnvironmentTypes {
		result.EnvironmentTypes = append(result.EnvironmentTypes, envTypeFromDbEnvType(&envType))
	}
	for _, module := range snapshot.Modules {
		result.Modules = append(result.Modules, moduleCatalogueToAPI(module))
	}
	for _, version := range snapshot.Versions {
		result.Versions = append(result.Versions, coreModuleVersionToAPI(version))
	}
	for _, pin := range snapshot.Pins {
		result.Pins = append(result.Pins, modulePinToAPI(pin))
	}
	for _, event := range snapshot.PinEvents {
		result.PinEvents = append(result.PinEvents, modulePinEventToAPI(event))
	}
	return result
}
