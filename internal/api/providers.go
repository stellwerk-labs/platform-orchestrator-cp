package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

const maximumAllowedSerializedConfig = 5 * 1024

func apiMpFromDbMp(mp model.ModuleProvider) ModuleProvider {
	return ModuleProvider{
		OrgId:             mp.OrgId,
		Configuration:     mp.Config,
		CreatedAt:         mp.CreatedAt,
		Description:       mp.Description.Ref(),
		Id:                mp.Id,
		ProviderType:      mp.ProviderType,
		Source:            mp.Source,
		VersionConstraint: mp.VersionConstraint,
	}
}

func apiMpSummaryFromDbMp(mp model.ModuleProvider) ModuleProviderSummary {
	return ModuleProviderSummary{
		OrgId:        mp.OrgId,
		CreatedAt:    mp.CreatedAt,
		Description:  mp.Description.Ref(),
		Id:           mp.Id,
		ProviderType: mp.ProviderType,
		Source:       mp.Source,
	}
}

func (s *Server) ListModuleProviders(ctx context.Context, request ListModuleProvidersRequestObject) (ListModuleProvidersResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListModuleProviders(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListModuleProvidersParams{
		ByType: request.Params.ByProviderType,
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListModuleProviders404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list module providers")
	}
	out := make([]ModuleProviderSummary, len(page))
	for i, item := range page {
		out[i] = apiMpSummaryFromDbMp(item)
	}
	return ListModuleProviders200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func (s *Server) CreateModuleProvider(ctx context.Context, request CreateModuleProviderRequestObject) (CreateModuleProviderResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())
	if request.Body.Configuration == nil {
		request.Body.Configuration = map[string]interface{}{}
	} else {
		raw, _ := json.Marshal(request.Body.Configuration)
		if len(raw) > maximumAllowedSerializedConfig {
			return CreateModuleProvider400JSONResponse{N400BadRequestJSONResponse: Generate400Response("provider configuration is too large")}, nil
		}
	}

	if err := ValidatePlaceholderSyntax(request.Body.Configuration, PlaceholdersSupportedInProvider); err != nil {
		return CreateModuleProvider400JSONResponse{N400BadRequestJSONResponse: Generate400Response("invalid configuration: " + err.Error())}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		if _, err := s.Database.GetOrg(ctx, tx, request.OrgId); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return CreateModuleProvider409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict(me.Message))}, nil
			}
			return nil, errors.Wrap(err, "failed to get internal org")
		}

		// NOTE: most validation is happening the open api schema
		if res, err := s.Database.CreateModuleProvider(ctx, tx, &model.ModuleProvider{
			OrgId:             request.OrgId,
			ProviderType:      request.Body.ProviderType,
			Id:                request.Body.Id,
			CreatedAt:         time.Now().UTC(),
			Description:       opt.OfRef(request.Body.Description),
			Source:            request.Body.Source,
			VersionConstraint: request.Body.VersionConstraint,
			Config:            request.Body.Configuration,
		}); err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return CreateModuleProvider409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create module provider")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		} else {
			return CreateModuleProvider201JSONResponse(apiMpFromDbMp(*res)), nil
		}
	}

}

func (s *Server) DeleteModuleProvider(ctx context.Context, request DeleteModuleProviderRequestObject) (DeleteModuleProviderResponseObject, error) {
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
		if err := s.Database.DeleteModuleProvider(ctx, tx, request.OrgId, request.ProviderType, request.ProviderId); err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return DeleteModuleProvider409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			} else if me, ok := model.IsErrNotFound(err); ok {
				return DeleteModuleProvider404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to delete module provider")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}
	}
	logger.Info("deleted module provider")
	return DeleteModuleProvider204Response{}, nil
}

func (s *Server) GetModuleProvider(ctx context.Context, request GetModuleProviderRequestObject) (GetModuleProviderResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	if mp, err := s.Database.GetModuleProvider(ctx, nil, request.OrgId, request.ProviderType, request.ProviderId); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetModuleProvider404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get module provider")
	} else {
		return GetModuleProvider200JSONResponse(apiMpFromDbMp(*mp)), nil
	}
}

func (s *Server) UpdateModuleProvider(ctx context.Context, request UpdateModuleProviderRequestObject) (UpdateModuleProviderResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	if err := ValidatePlaceholderSyntax(ref.DerefOr(request.Body.Configuration, map[string]interface{}{}), PlaceholdersSupportedInProvider); err != nil {
		return UpdateModuleProvider400JSONResponse{N400BadRequestJSONResponse: Generate400Response("invalid configuration: " + err.Error())}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		current, err := s.Database.GetModuleProvider(ctx, tx, request.OrgId, request.ProviderType, request.ProviderId)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateModuleProvider404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get module provider")
		}

		if request.Body.VersionConstraint != nil {
			current.VersionConstraint = *request.Body.VersionConstraint
		}
		if request.Body.Description != nil {
			current.Description = opt.OfRef(request.Body.Description)
		}
		if request.Body.Configuration != nil {
			current.Config = *request.Body.Configuration
		}

		// NOTE: most validation is happening the open api schema
		if res, err := s.Database.UpdateModuleProvider(ctx, tx, current); err != nil {
			return nil, errors.Wrap(err, "failed to update module provider")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		} else {
			return UpdateModuleProvider200JSONResponse(apiMpFromDbMp(*res)), nil
		}
	}
}
