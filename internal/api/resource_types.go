package api

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

var reservedResourceTypes = []string{
	// workload is an internal resource type used in the graph. Customers should avoid overlapping with this because
	// it may cause confusion or in some cases unexpected behavior.
	"workload",
}

func apiResourceTypeFromModelResourceType(resourceType model.ResourceType) ResourceType {
	return ResourceType{
		Id:                    resourceType.Id,
		Description:           ref.RefStringEmptyNil(resourceType.Description),
		OutputSchema:          resourceType.OutputsSchema,
		CreatedAt:             resourceType.CreatedAt,
		BuiltIn:               !resourceType.OrgId.IsSet(),
		IsDeveloperAccessible: resourceType.IsDeveloperAccessible,
	}
}

// InternalListResourceTypes List built-in resource types
// (GET /internal/resource-types)
func (s *Server) InternalListResourceTypes(ctx context.Context, request InternalListResourceTypesRequestObject) (InternalListResourceTypesResponseObject, error) {
	page, next, err := s.Database.ListResourceTypes(ctx, nil, nil, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100))
	if err != nil {
		return nil, errors.Wrap(err, "failed to list resource types")
	}
	out := make([]ResourceType, len(page))
	for i, item := range page {
		out[i] = apiResourceTypeFromModelResourceType(item)
	}
	return InternalListResourceTypes200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

// InternalCreateResourceType Create a new built-in resource type
// (POST /internal/resource-types)
func (s *Server) InternalCreateResourceType(ctx context.Context, request InternalCreateResourceTypeRequestObject) (InternalCreateResourceTypeResponseObject, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx)
	resType, err := s.Database.CreateResourceType(ctx, nil, &model.ResourceType{
		Id:                    request.Body.Id,
		Description:           ref.DerefOr(request.Body.Description, ""),
		OutputsSchema:         request.Body.OutputSchema,
		CreatedAt:             time.Now().UTC(),
		IsDeveloperAccessible: ref.DerefOr(request.Body.IsDeveloperAccessible, true),
	})
	if err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return InternalCreateResourceType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to create resource type")
	}

	logger.Info("created built-in resource type", zap.String("resource_type_id", request.Body.Id))
	return InternalCreateResourceType201JSONResponse(apiResourceTypeFromModelResourceType(*resType)), nil
}

// InternalDeleteResourceType Delete a built-in resource type
// (DELETE /internal/resource-types/{type})
func (s *Server) InternalDeleteResourceType(ctx context.Context, request InternalDeleteResourceTypeRequestObject) (InternalDeleteResourceTypeResponseObject, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx)
	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()
		if err := s.Database.DeleteResourceType(ctx, tx, nil, request.TypeId); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return InternalDeleteResourceType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			} else if me, ok := model.IsErrConflict(err); ok {
				return InternalDeleteResourceType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to delete resource type "+request.TypeId)
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}
	}
	logger.Info("deleted built-in resource type", zap.String("resource_type_id", request.TypeId))
	return InternalDeleteResourceType204Response{}, nil
}

// InternalUpdateResourceType Update a built-in resource type
// (PATCH /internal/resource-types/{type})
func (s *Server) InternalUpdateResourceType(ctx context.Context, request InternalUpdateResourceTypeRequestObject) (InternalUpdateResourceTypeResponseObject, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx)
	// NOTE: most validation is happening the open api schema
	res, err := s.Database.UpdateResourceType(ctx, nil, nil, request.TypeId, &model.ResourceTypePatch{
		Description:           opt.OfRef(request.Body.Description),
		OutputsSchema:         request.Body.OutputSchema,
		IsDeveloperAccessible: opt.OfRef(request.Body.IsDeveloperAccessible),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return InternalUpdateResourceType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to update resource type")
	}
	logger.Info("updated built-in resource type", zap.String("resource_type_id", request.TypeId))
	return InternalUpdateResourceType200JSONResponse(apiResourceTypeFromModelResourceType(*res)), nil
}

// ListResourceTypes List available resource types
// (GET /orgs/{orgId}/resource-types)
func (s *Server) ListResourceTypes(ctx context.Context, request ListResourceTypesRequestObject) (ListResourceTypesResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListResourceTypes(ctx, nil, &request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100))
	if err != nil {
		return nil, errors.Wrap(err, "failed to list resource types")
	}
	out := make([]ResourceType, len(page))
	for i, item := range page {
		out[i] = apiResourceTypeFromModelResourceType(item)
	}
	return ListResourceTypes200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

// CreateResourceType Create a new resource type
// (POST /orgs/{orgId}/resource-types)
func (s *Server) CreateResourceType(ctx context.Context, request CreateResourceTypeRequestObject) (CreateResourceTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	if slices.Contains(reservedResourceTypes, request.Body.Id) {
		return CreateResourceType400JSONResponse{N400BadRequestJSONResponse: Generate400Response(fmt.Sprintf("resource type id '%s' is reserved", request.Body.Id))}, nil
	}

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	if _, err := s.Database.GetOrg(ctx, tx, request.OrgId); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return CreateResourceType400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(model.NewErrBadRequest(me.Message))}, nil
		}
		return nil, errors.Wrap(err, "failed to get internal org")
	}

	resType, err := s.Database.CreateResourceType(ctx, tx, &model.ResourceType{
		OrgId:                 opt.Of(request.OrgId),
		Id:                    request.Body.Id,
		Description:           ref.DerefOr(request.Body.Description, ""),
		OutputsSchema:         request.Body.OutputSchema,
		CreatedAt:             time.Now().UTC(),
		IsDeveloperAccessible: ref.DerefOr(request.Body.IsDeveloperAccessible, true),
	})
	if err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return CreateResourceType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to create resource type")
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}
	logger.Info("created resource type", zap.String("resource_type_id", request.Body.Id))
	return CreateResourceType201JSONResponse(apiResourceTypeFromModelResourceType(*resType)), nil
}

// DeleteResourceType Delete a resource type
// (DELETE /orgs/{orgId}/resource-types/{type})
func (s *Server) DeleteResourceType(ctx context.Context, request DeleteResourceTypeRequestObject) (DeleteResourceTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())
	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()
		if err := s.Database.DeleteResourceType(ctx, tx, &request.OrgId, request.TypeId); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return DeleteResourceType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			} else if me, ok := model.IsErrConflict(err); ok {
				return DeleteResourceType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to delete resource type "+request.TypeId)
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}
	}
	logger.Info("deleted resource type", zap.String("resource_type_id", request.TypeId))
	return DeleteResourceType204Response{}, nil
}

// GetResourceType Get a resource type
// (GET /orgs/{orgId}/resource-types/{type})
func (s *Server) GetResourceType(ctx context.Context, request GetResourceTypeRequestObject) (GetResourceTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	resType, err := s.Database.GetResourceType(ctx, nil, &request.OrgId, request.TypeId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetResourceType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get resource type")
	}
	return GetResourceType200JSONResponse(apiResourceTypeFromModelResourceType(*resType)), nil
}

// UpdateResourceType Update a resource type
// (PATCH /orgs/{orgId}/resource-types/{type})
func (s *Server) UpdateResourceType(ctx context.Context, request UpdateResourceTypeRequestObject) (UpdateResourceTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err = tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	if _, err = s.Database.GetOrg(ctx, tx, request.OrgId); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateResourceType400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(model.NewErrBadRequest(me.Message))}, nil
		}
		return nil, errors.Wrap(err, "failed to get internal org")
	}

	// NOTE: most validation is happening the open api schema
	res, err := s.Database.UpdateResourceType(ctx, tx, &request.OrgId, request.TypeId, &model.ResourceTypePatch{
		Description:           opt.OfRef(request.Body.Description),
		OutputsSchema:         request.Body.OutputSchema,
		IsDeveloperAccessible: opt.OfRef(request.Body.IsDeveloperAccessible),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateResourceType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to update resource type")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}
	logger.Info("updated resource type", zap.String("resource_type_id", request.TypeId))
	return UpdateResourceType200JSONResponse(apiResourceTypeFromModelResourceType(*res)), nil
}
