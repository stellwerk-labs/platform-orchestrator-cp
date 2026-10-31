package api

import (
	"context"
	"fmt"
	"strconv"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func resourceTypeVersionToAPI(v model.ResourceTypeVersion) ResourceTypeVersion {
	return ResourceTypeVersion{Uuid: v.UUID, ResourceTypeUuid: v.ResourceTypeUUID, ResourceTypeId: v.ResourceTypeID,
		Version: v.Version, OutputSchema: v.OutputSchema, ModuleContract: optionalJSONObject[ResourceTypeModuleContract](v.ModuleContract),
		CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, Reason: v.Reason}
}

func (s *Server) GetResourceTypeVersion(ctx context.Context, request GetResourceTypeVersionRequestObject) (GetResourceTypeVersionResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err = s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionResourceTypeRead); err != nil {
		return nil, err
	}
	v, err := s.Database.GetResourceTypeVersion(ctx, nil, request.OrgId, request.TypeId, request.ResourceTypeVersion)
	if missing, ok := model.IsErrNotFound(err); ok {
		return GetResourceTypeVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(missing)}, nil
	}
	if err != nil {
		return nil, err
	}
	return GetResourceTypeVersion200JSONResponse(resourceTypeVersionToAPI(*v)), nil
}

func (s *Server) ListResourceTypeVersions(ctx context.Context, request ListResourceTypeVersionsRequestObject) (ListResourceTypeVersionsResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err = s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionResourceTypeRead); err != nil {
		return nil, err
	}
	if _, err = s.Database.GetResourceType(ctx, nil, &request.OrgId, request.TypeId); err != nil {
		if missing, ok := model.IsErrNotFound(err); ok {
			return ListResourceTypeVersions404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(missing)}, nil
		}
		return nil, err
	}
	after := int64(0)
	if request.Params.Page != nil {
		after, err = strconv.ParseInt(*request.Params.Page, 10, 64)
	}
	limit := ref.DerefOr(request.Params.PerPage, 50)
	if err != nil || after < 0 || limit < 1 || limit > 100 {
		return ListResourceTypeVersions400JSONResponse{N400BadRequestJSONResponse: Generate400Response("invalid Resource Type version page")}, nil
	}
	versions, err := s.Database.ListResourceTypeVersions(ctx, nil, request.OrgId, request.TypeId, after, limit+1)
	if err != nil {
		return nil, err
	}
	response := ListResourceTypeVersions200JSONResponse{Items: make([]ResourceTypeVersion, 0)}
	if len(versions) > limit {
		versions = versions[:limit]
		response.NextPageToken = ref.Ref(strconv.FormatInt(versions[len(versions)-1].Version, 10))
	}
	for _, v := range versions {
		response.Items = append(response.Items, resourceTypeVersionToAPI(v))
	}
	return response, nil
}

func (s *Server) PublishResourceTypeVersion(ctx context.Context, request PublishResourceTypeVersionRequestObject) (PublishResourceTypeVersionResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	// Re-authorize every request, including a cached idempotent replay.
	if err = s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionResourceTypeWrite); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("resource-type-version:%s", request.TypeId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ResourceTypeVersion](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishResourceTypeVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return PublishResourceTypeVersion201JSONResponse(response), nil
	}
	v, err := s.Database.PublishResourceTypeVersion(ctx, tx, request.OrgId, request.TypeId, model.ResourceTypeVersionPublication{Version: request.Body.Version, ExpectedResourceVersion: request.Body.ExpectedResourceVersion, OutputSchema: request.Body.OutputSchema, ModuleContract: ref.DerefOr(request.Body.ModuleContract, nil), Actor: actor, Reason: request.Body.Reason})
	if err != nil {
		if bad, ok := model.IsErrBadRequest(err); ok {
			return PublishResourceTypeVersion400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(bad)}, nil
		}
		if missing, ok := model.IsErrNotFound(err); ok {
			return PublishResourceTypeVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(missing)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishResourceTypeVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := resourceTypeVersionToAPI(*v)
	if err = s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return PublishResourceTypeVersion201JSONResponse(response), nil
}
