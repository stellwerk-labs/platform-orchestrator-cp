package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/hmessaging/reliableoutbox"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

func envTypeFromDbEnvType(et *model.EnvType) EnvironmentType {
	return EnvironmentType{
		Id:          et.Id,
		Uuid:        et.Uuid,
		DisplayName: et.DisplayName,
		CreatedAt:   et.CreatedAt,
	}
}

func (s *Server) ListEnvironmentTypes(ctx context.Context, request ListEnvironmentTypesRequestObject) (ListEnvironmentTypesResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListEnvironmentTypes(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100))
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListEnvironmentTypes404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list environment types")
	}
	out := make([]EnvironmentType, len(page))
	for i, item := range page {
		out[i] = envTypeFromDbEnvType(&item)
	}
	return ListEnvironmentTypes200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func (s *Server) CreateEnvironmentType(ctx context.Context, request CreateEnvironmentTypeRequestObject) (CreateEnvironmentTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapEnvTypeId(request.Body.Id))

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	org, err := s.Database.GetOrg(ctx, tx, request.OrgId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return CreateEnvironmentType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict(me.Message))}, nil
		}
		return nil, errors.Wrap(err, "failed to get organization")
	}

	et, err := s.Database.CreateEnvironmentType(ctx, tx, &model.EnvType{OrgId: request.OrgId, OrgUuid: org.Uuid, Id: request.Body.Id, DisplayName: ref.DerefOr(request.Body.DisplayName, request.Body.Id), CreatedAt: time.Now().UTC()})
	if err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return CreateEnvironmentType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to create environment type")
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentTypeCreated,
		Time: et.CreatedAt,
		Data: genevents.EnvTypeChangedData{OrgId: request.OrgId, OrgUuid: et.OrgUuid, EnvTypeId: et.Id, EnvTypeUuid: et.Uuid},
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	logger.Info("created environment type")
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return CreateEnvironmentType201JSONResponse(envTypeFromDbEnvType(et)), nil
}

func (s *Server) UpdateEnvironmentType(ctx context.Context, request UpdateEnvironmentTypeRequestObject) (UpdateEnvironmentTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}

	if request.Body == nil {
		out, err := s.Database.GetEnvironmentType(ctx, nil, request.OrgId, request.EnvTypeId, model.GetModeDefault)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateEnvironmentType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get environment type")
		}
		return UpdateEnvironmentType200JSONResponse(envTypeFromDbEnvType(out)), nil
	}

	out, err := s.Database.UpdateEnvironmentType(ctx, nil, request.OrgId, request.EnvTypeId, model.UpdateEnvTypeParams{
		DisplayName: opt.Of(request.Body.DisplayName),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateEnvironmentType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to update environment type")
	}
	return UpdateEnvironmentType200JSONResponse(envTypeFromDbEnvType(out)), nil
}

func (s *Server) DeleteEnvironmentType(ctx context.Context, request DeleteEnvironmentTypeRequestObject) (DeleteEnvironmentTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapEnvTypeId(request.EnvTypeId))

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	et, err := s.Database.GetEnvironmentType(ctx, tx, request.OrgId, request.EnvTypeId, model.GetModeForUpdate)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return DeleteEnvironmentType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	}

	if err := s.Database.DeleteEnvironmentType(ctx, tx, request.OrgId, request.EnvTypeId); err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return DeleteEnvironmentType409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, err
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentTypeDeleted,
		Time: time.Now().UTC(),
		Data: genevents.EnvTypeChangedData{OrgId: request.OrgId, OrgUuid: et.OrgUuid, EnvTypeId: et.Id, EnvTypeUuid: et.Uuid},
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	logger.Info("environment type deleted")
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return DeleteEnvironmentType204Response{}, nil
}

func (s *Server) GetEnvironmentType(ctx context.Context, request GetEnvironmentTypeRequestObject) (GetEnvironmentTypeResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	if out, err := s.Database.GetEnvironmentType(ctx, nil, request.OrgId, request.EnvTypeId, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetEnvironmentType404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get env type")
	} else {
		return GetEnvironmentType200JSONResponse(envTypeFromDbEnvType(out)), nil
	}
}
