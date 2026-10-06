package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/herrors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func (s *Server) ListPluginContributions(ctx context.Context, request ListPluginContributionsRequestObject) (ListPluginContributionsResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionOrganizationRead); err != nil {
		return nil, err
	}
	response := ListPluginContributions200JSONResponse{ContractLevel: PluginContractLevelV1alpha, Items: []PluginContribution{}}
	if s.PluginGateway == nil || s.PluginGateway.Registry == nil {
		return response, nil
	}
	for _, descriptor := range s.PluginGateway.Registry.List(ctx, request.OrgId) {
		installation, _, _ := s.PluginGateway.Registry.Installation(ctx, request.OrgId, descriptor.ID)
		operations := []map[string]any{}
		for _, op := range installation.Operations {
			if descriptor.Allows(op.RequiredFeature, op.Mutating, op.Safety || len(op.SafetyPathValues) > 0) {
				operations = append(operations, map[string]any{"id": op.ID, "method": op.Method, "mutating": op.Mutating, "required_feature": op.RequiredFeature})
			}
		}
		value := map[string]any{"id": descriptor.ID, "display_name": descriptor.DisplayName, "version": descriptor.Version, "state": descriptor.State, "mode": descriptor.Mode, "operations": operations}
		if descriptor.Entitlement != nil {
			value["entitlement"] = descriptor.Entitlement
		}
		if descriptor.Mode != plugins.ModeHidden && installation.UI != nil {
			if descriptor.Mode != plugins.ModeFull {
				installation.UI.ContextualActions = nil
			}
			value["ui"] = installation.UI
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var item PluginContribution
		if err := json.Unmarshal(encoded, &item); err != nil {
			return nil, err
		}
		response.Items = append(response.Items, item)
	}
	return response, nil
}

func (s *Server) InvokePluginOperation(ctx context.Context, request InvokePluginOperationRequestObject) (InvokePluginOperationResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionOrganizationRead); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, echo.NewHTTPError(400, "A plugin invocation is required.")
	}
	body, err := json.Marshal(request.Body.Body)
	if err != nil {
		return nil, err
	}
	result, err := s.PluginGateway.Invoke(ctx, request.OrgId, request.PluginId, actor.String(), plugins.Invocation{
		OperationID: request.Body.OperationId, PathParameters: ref.DerefOr(request.Body.PathParameters, map[string]string{}), Query: ref.DerefOr(request.Body.Query, map[string][]string{}), Body: body, IdempotencyKey: ref.DerefOr(request.Body.IdempotencyKey, ""),
	})
	if err != nil {
		var unavailable *plugins.GatewayError
		if errors.As(err, &unavailable) {
			return nil, &herrors.PlatformOrchestratorError{StatusCode: unavailable.Status, Code: unavailable.Code, Message: unavailable.Message}
		}
		return nil, err
	}
	return InvokePluginOperation200JSONResponse{Status: result.Status, Body: result.Body}, nil
}

func (s *Server) GetPluginCapability(
	ctx context.Context,
	request GetPluginCapabilityRequestObject,
) (GetPluginCapabilityResponseObject, error) {
	uid, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionOrganizationRead); err != nil {
		return GetPluginCapability403JSONResponse{}, err
	}

	descriptor, found := plugins.Descriptor{}, false
	if s.Plugins != nil {
		descriptor, found = s.Plugins.Get(ctx, request.OrgId, request.PluginId)
	}
	if !found {
		descriptor = plugins.Descriptor{
			ID:          request.PluginId,
			State:       plugins.StateNotInstalled,
			AdminAction: "Ask an organization administrator to install the required plugin.",
		}
	}

	return GetPluginCapability200JSONResponse{
		ContractLevel: PluginContractLevelV1alpha,
		Id:            descriptor.ID,
		DisplayName:   descriptor.DisplayName,
		Version:       ref.RefStringEmptyNil(descriptor.Version),
		State:         PluginAvailabilityState(descriptor.State),
		Capabilities:  descriptor.Capabilities,
		AdminAction:   ref.RefStringEmptyNil(descriptor.AdminAction),
		Entitlement:   pluginEntitlementToAPI(descriptor.Entitlement),
	}, nil
}

func pluginEntitlementToAPI(status *plugins.EntitlementStatus) *PluginEntitlementStatus {
	if status == nil {
		return nil
	}
	result := &PluginEntitlementStatus{Features: map[string]PluginFeatureAccess{}}
	for feature, access := range status.Features {
		result.Features[feature] = PluginFeatureAccess{State: access.State, TrustClass: access.TrustClass, Forward: access.Forward, Read: access.Read, Safety: access.Safety, ExpiresAt: access.ExpiresAt, GraceUntil: access.GraceUntil}
	}
	return result
}
