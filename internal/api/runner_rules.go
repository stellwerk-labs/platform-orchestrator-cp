package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func apiRunnerRuleFromDbRunnerRule(r model.RunnerRule) RunnerRule {
	return RunnerRule(apiRunnerRuleSummaryFromDbRunnerRule(r))
}

func apiRunnerRuleSummaryFromDbRunnerRule(r model.RunnerRule) RunnerRuleSummary {
	return RunnerRuleSummary{
		CreatedAt: r.CreatedAt,
		RunnerId:  r.RunnerId,
		Id:        r.Id,
		OrgId:     r.OrgId,
		ProjectId: r.ProjectId.Or(""),
		EnvTypeId: r.EnvTypeId.Or(""),
	}
}

func (s *Server) ListRunnerRulesInOrg(ctx context.Context, request ListRunnerRulesInOrgRequestObject) (ListRunnerRulesInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRuleRead); err != nil {
		return nil, err
	}
	if page, next, err := s.Database.ListRunnerRules(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListRunnerRulesParams{
		EffectiveInProjectId: request.Params.ByProjectId,
		EffectiveInEnvTypeId: request.Params.ByEnvTypeId,
		ByRunnerId:           request.Params.ByRunnerId,
	}); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListRunnerRulesInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list runner rules")
	} else {
		out := make([]RunnerRuleSummary, len(page))
		for i, item := range page {
			out[i] = apiRunnerRuleSummaryFromDbRunnerRule(item)
		}
		return ListRunnerRulesInOrg200JSONResponse{
			Items:         out,
			NextPageToken: ref.RefStringEmptyNil(next),
		}, nil
	}
}

func (s *Server) CreateRunnerRuleInOrg(ctx context.Context, request CreateRunnerRuleInOrgRequestObject) (CreateRunnerRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRuleWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapRunnerId(request.Body.RunnerId)).WithLazy(ids.AsLogField())

	if request.Body.RunnerId == "" {
		return CreateRunnerRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400Response("runner_id is mandatory to create a runner rule")}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()
		var envTypeId, projectId *string
		if ref.DerefOr(request.Body.ProjectId, "") != "" {
			projectId = request.Body.ProjectId
		}
		if ref.DerefOr(request.Body.EnvTypeId, "") != "" {
			envTypeId = request.Body.EnvTypeId
		}
		if res, err := s.Database.CreateRunnerRule(ctx, tx, request.OrgId, &model.RunnerRule{
			OrgId:     request.OrgId,
			RunnerId:  request.Body.RunnerId,
			CreatedAt: time.Now().UTC(),
			ProjectId: opt.OfRef(projectId),
			EnvTypeId: opt.OfRef(envTypeId),
		}); err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return CreateRunnerRuleInOrg409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			} else if me, ok := model.IsErrBadRequest(err); ok {
				return CreateRunnerRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create module rule")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		} else {
			logger = logger.With(logging.ZapRunnerRuleId(res.Id.String()))
			logger.Info("created runner rule")
			return CreateRunnerRuleInOrg201JSONResponse(apiRunnerRuleFromDbRunnerRule(*res)), nil
		}
	}
}

func (s *Server) DeleteRunnerRuleInOrg(ctx context.Context, request DeleteRunnerRuleInOrgRequestObject) (DeleteRunnerRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRuleWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapRunnerRuleId(request.RuleId.String())).WithLazy(ids.AsLogField())
	if err := s.Database.DeleteRunnerRule(ctx, nil, request.OrgId, request.RuleId.String()); err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return DeleteRunnerRuleInOrg409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		} else if me, ok := model.IsErrNotFound(err); ok {
			return DeleteRunnerRuleInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to delete runner rule")
	}
	logger.Info("deleted runner rule")
	return DeleteRunnerRuleInOrg204Response{}, nil
}

func (s *Server) GetRunnerRuleInOrg(ctx context.Context, request GetRunnerRuleInOrgRequestObject) (GetRunnerRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerRuleRead); err != nil {
		return nil, err
	}
	if res, err := s.Database.GetRunnerRule(ctx, nil, request.OrgId, request.RuleId.String()); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetRunnerRuleInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get runner rule")
	} else {
		return GetRunnerRuleInOrg200JSONResponse(apiRunnerRuleFromDbRunnerRule(*res)), nil
	}
}
