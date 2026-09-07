package api

import (
	"context"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

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
		Id:           descriptor.ID,
		DisplayName:  descriptor.DisplayName,
		Version:      ref.RefStringEmptyNil(descriptor.Version),
		State:        PluginAvailabilityState(descriptor.State),
		Capabilities: descriptor.Capabilities,
		AdminAction:  ref.RefStringEmptyNil(descriptor.AdminAction),
	}, nil
}
