package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

const DefaultResourceClass = "default"

func apiRuleFromDbRule(r model.DefinitionRule) Rule {
	return Rule(apiRuleSummaryFromDbRule(r))
}

func apiRuleSummaryFromDbRule(r model.DefinitionRule) RuleSummary {
	return RuleSummary{
		CreatedAt:     r.CreatedAt,
		ModuleId:      r.DefinitionId,
		Id:            r.Id,
		OrgId:         r.OrgId,
		ResourceType:  r.ResourceType,
		ResourceClass: r.ResourceClass,
		ResourceId:    r.ResourceId.Ref(),
		ProjectId:     r.ProjectId.Ref(),
		EnvTypeId:     r.EnvTypeId.Ref(),
		EnvId:         r.EnvId.Ref(),
	}
}

func (s *Server) ListModuleRulesInOrg(ctx context.Context, request ListModuleRulesInOrgRequestObject) (ListModuleRulesInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}

	if request.Params.ByEnvId != nil && request.Params.ByProjectId == nil {
		return ListModuleRulesInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400Response("by_env_id: must be used together with by_project_id")}, nil
	}

	if page, next, err := s.Database.ListModuleRules(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListModuleRulesParams{
		ByResourceType: request.Params.ByResourceType,
		ByDefinitionId: request.Params.ByModuleId,
		ByProjectId:    request.Params.ByProjectId,
		ByEnvId:        request.Params.ByEnvId,
	}); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListModuleRulesInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list module rules")
	} else {
		out := make([]RuleSummary, len(page))
		for i, item := range page {
			out[i] = apiRuleSummaryFromDbRule(item)
		}
		return ListModuleRulesInOrg200JSONResponse{
			Items:         out,
			NextPageToken: ref.RefStringEmptyNil(next),
		}, nil
	}
}

func (s *Server) CreateModuleRuleInOrg(ctx context.Context, request CreateModuleRuleInOrgRequestObject) (CreateModuleRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapModuleDefinitionId(request.Body.ModuleId))

	if request.Body.EnvId != nil {
		if request.Body.ProjectId == nil {
			return CreateModuleRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400Response("project_id: must be set when env_id is set")}, nil
		} else if request.Body.EnvTypeId != nil {
			return CreateModuleRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400Response("env_type_id: must not be set when env_id is set")}, nil
		}
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		definition, err := s.Database.GetModuleDefinition(ctx, tx, request.OrgId, request.Body.ModuleId, model.GetModeForUpdate)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return CreateModuleRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(model.NewErrBadRequest(me.Message))}, nil
			}
			return nil, errors.Wrap(err, "failed to get module definition")
		}

		// getting the definition id and resource type happens within this db method
		if res, err := s.Database.CreateModuleRule(ctx, tx, request.OrgId, &model.DefinitionRule{
			OrgId:         request.OrgId,
			DefinitionId:  definition.DefinitionId,
			CreatedAt:     time.Now().UTC(),
			ResourceType:  definition.ResourceType,
			ResourceClass: ref.DerefOr(request.Body.ResourceClass, DefaultResourceClass),
			ResourceId:    opt.OfRef(request.Body.ResourceId),
			EnvTypeId:     opt.OfRef(request.Body.EnvTypeId),
			ProjectId:     opt.OfRef(request.Body.ProjectId),
			EnvId:         opt.OfRef(request.Body.EnvId),
		}); err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return CreateModuleRuleInOrg409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			} else if me, ok := model.IsErrBadRequest(err); ok {
				return CreateModuleRuleInOrg400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create module rule")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		} else {
			logger = logger.With(logging.ZapModuleRuleId(res.Id.String()))
			logger.Info("created module rule")
			return CreateModuleRuleInOrg201JSONResponse(apiRuleFromDbRule(*res)), nil
		}
	}
}

func (s *Server) DeleteModuleRuleInOrg(ctx context.Context, request DeleteModuleRuleInOrgRequestObject) (DeleteModuleRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapModuleRuleId(request.RuleId.String()))
	if err := s.Database.DeleteModuleRule(ctx, nil, request.OrgId, request.RuleId.String()); err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return DeleteModuleRuleInOrg409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		} else if me, ok := model.IsErrNotFound(err); ok {
			return DeleteModuleRuleInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to delete module rule")
	}
	logger.Info("deleted module rule")
	return DeleteModuleRuleInOrg204Response{}, nil
}

func (s *Server) GetModuleRuleInOrg(ctx context.Context, request GetModuleRuleInOrgRequestObject) (GetModuleRuleInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	if res, err := s.Database.GetModuleRule(ctx, nil, request.OrgId, request.RuleId.String()); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetModuleRuleInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get module rule")
	} else {
		return GetModuleRuleInOrg200JSONResponse(apiRuleFromDbRule(*res)), nil
	}
}
