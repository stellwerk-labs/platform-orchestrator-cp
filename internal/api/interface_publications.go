package api

import (
	"context"
	"net/http"
)

// These routes distinguish explicit interface selection from legacy publication
// handlers that would silently discard an unknown JSON field. Older servers
// cannot execute the command because they do not expose the route.
type interfacePublicationResponse struct {
	response PublishModuleVersionResponseObject
}

func (r interfacePublicationResponse) VisitPublishModuleVersionWithInterfaceResponse(w http.ResponseWriter) error {
	return r.response.VisitPublishModuleVersionResponse(w)
}

func (s *Server) PublishModuleVersionWithInterface(ctx context.Context, request PublishModuleVersionWithInterfaceRequestObject) (PublishModuleVersionWithInterfaceResponseObject, error) {
	if _, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionPublish); err != nil {
		return nil, err
	}
	if request.Body == nil || request.Body.ResourceTypeVersion == nil {
		return PublishModuleVersionWithInterface400JSONResponse{N400BadRequestJSONResponse: Generate400Response("resource_type_version is required for an explicit interface publication")}, nil
	}
	response, err := s.PublishModuleVersion(ctx, PublishModuleVersionRequestObject{OrgId: request.OrgId, ModuleId: request.ModuleId,
		Body: request.Body, Params: PublishModuleVersionParams{IdempotencyKey: request.Params.IdempotencyKey}})
	if err != nil {
		return nil, err
	}
	return interfacePublicationResponse{response: response}, nil
}

type interfaceSuccessorResponse struct {
	response PublishStableModuleVersionSuccessorResponseObject
}

func (r interfaceSuccessorResponse) VisitPublishStableModuleVersionSuccessorWithInterfaceResponse(w http.ResponseWriter) error {
	return r.response.VisitPublishStableModuleVersionSuccessorResponse(w)
}

func (s *Server) PublishStableModuleVersionSuccessorWithInterface(ctx context.Context, request PublishStableModuleVersionSuccessorWithInterfaceRequestObject) (PublishStableModuleVersionSuccessorWithInterfaceResponseObject, error) {
	if _, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionPublish); err != nil {
		return nil, err
	}
	if request.Body == nil || request.Body.Version.ResourceTypeVersion == nil {
		return PublishStableModuleVersionSuccessorWithInterface400JSONResponse{N400BadRequestJSONResponse: Generate400Response("version.resource_type_version is required for an explicit interface publication")}, nil
	}
	response, err := s.PublishStableModuleVersionSuccessor(ctx, PublishStableModuleVersionSuccessorRequestObject{OrgId: request.OrgId, ModuleId: request.ModuleId,
		ModuleVersionId: request.ModuleVersionId, Body: request.Body,
		Params: PublishStableModuleVersionSuccessorParams{IdempotencyKey: request.Params.IdempotencyKey}})
	if err != nil {
		return nil, err
	}
	return interfaceSuccessorResponse{response: response}, nil
}
