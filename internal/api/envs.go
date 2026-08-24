package api

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/hmessaging/reliableoutbox"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stellwerk-labs/golib/htelemetry"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

const (
	getRunnerRulesPaginationSize = 200
)

func calculateRunnerRuleSpecificity(bits ...opt.Opt[string]) int {
	n := 0
	for _, bit := range bits {
		n <<= 1
		if bit.IsSet() {
			n++
		}
	}
	return n
}

type RunnerRuleWithSpecificity struct {
	Rule        model.RunnerRule
	Specificity int
}

func (s *Server) getMatchingRunners(ctx context.Context, tx model.Tx, orgId, projectId, envTypeId string) ([]RunnerRuleWithSpecificity, error) {
	span, ctx := htelemetry.StartSpanFromContext(ctx, "list-runner-rules")
	defer span.Finish()
	var runnerRules []RunnerRuleWithSpecificity

	for pageToken, pageNum := "", 1; pageNum == 1 || pageToken != ""; pageNum++ {
		if page, nextPageToken, err := s.Database.ListRunnerRules(ctx, tx, orgId, pageToken, getRunnerRulesPaginationSize, model.ListRunnerRulesParams{
			EffectiveInProjectId: &projectId,
			EffectiveInEnvTypeId: &envTypeId,
		}); err != nil {
			span.Finish(htelemetry.WithError(err))
			return nil, errors.Wrapf(err, "failed to list runner rules on page %d ", pageNum)
		} else {
			for _, rule := range page {
				runnerRules = append(runnerRules, RunnerRuleWithSpecificity{
					Rule:        rule,
					Specificity: calculateRunnerRuleSpecificity(rule.ProjectId, rule.EnvTypeId),
				})
			}
			pageToken = nextPageToken
		}
	}
	slices.SortFunc(runnerRules, func(a, b RunnerRuleWithSpecificity) int {
		return b.Specificity - a.Specificity
	})
	return runnerRules, nil
}

func envFromDbModel(e *model.Environment) Environment {
	return Environment{
		Id:            e.Id,
		ProjectId:     e.ProjectId,
		ProjectUuid:   ref.Ref(e.ProjectUuid),
		EnvTypeId:     e.EnvTypeId,
		Uuid:          e.Uuid,
		DisplayName:   e.DisplayName,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
		RunnerId:      e.RunnerId.Ref(),
		Status:        EnvironmentStatus(e.Status),
		StatusMessage: e.StatusMessage.Ref(),
	}
}

func envEventFromDbModel(e *model.Environment) genevents.EnvChangedData {
	return genevents.EnvChangedData{
		OrgId:       e.OrgId,
		OrgUuid:     e.OrgUuid,
		ProjectId:   e.ProjectId,
		ProjectUuid: e.ProjectUuid,
		EnvTypeId:   e.EnvTypeId,
		EnvTypeUuid: e.EnvTypeUuid,
		EnvId:       e.Id,
		EnvUuid:     e.Uuid,
		Status:      ref.RefStringEmptyNil(string(e.Status)),
	}
}

// List environments in a project
// (GET /orgs/{orgId}/apps/{projectId}/envs)
func (s *Server) ListEnvironments(ctx context.Context, request ListEnvironmentsRequestObject) (ListEnvironmentsResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionEnvironmentRead); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListEnvironments(
		ctx, nil, request.OrgId, request.ProjectId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100),
		model.ListEnvironmentsParams{
			ByEnvTypeIds: ref.DerefOr(request.Params.ByEnvTypeId, nil),
		},
	)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListEnvironments404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list environments")
	}
	out := make([]Environment, len(page))
	for i, item := range page {
		out[i] = envFromDbModel(&item)
	}
	return ListEnvironments200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

// List environments in an organization
// (GET /orgs/{orgId}/envs)
func (s *Server) ListEnvironmentsInOrg(ctx context.Context, request ListEnvironmentsInOrgRequestObject) (ListEnvironmentsInOrgResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionEnvironmentRead); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListEnvironmentsInOrg(
		ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100),
		model.ListEnvironmentsParams{
			ByEnvTypeIds: ref.DerefOr(request.Params.ByEnvTypeId, nil),
		},
	)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListEnvironmentsInOrg404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list environments")
	}
	out := make([]Environment, len(page))
	for i, item := range page {
		out[i] = envFromDbModel(&item)
	}
	return ListEnvironmentsInOrg200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func (s *Server) createEnvAndMessageInDatabase(
	ctx context.Context, tx model.Tx, proj *model.Project, envType *model.EnvType,
	envId, displayName string,
) (*model.Environment, []*hstandardoutbox.PendingEventMessage, error) {

	timeNow := time.Now().UTC()

	env, err := s.Database.CreateEnvironment(ctx, tx, &model.Environment{
		OrgId: proj.OrgId, OrgUuid: proj.OrgUuid,
		ProjectId: proj.Id, ProjectUuid: proj.Uuid,
		EnvTypeId: envType.Id, EnvTypeUuid: envType.Uuid,
		Id: envId, DisplayName: displayName, CreatedAt: timeNow, UpdatedAt: timeNow,
		Status: model.EnvironmentStatusActive,
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create environment")
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentCreated,
		Time: env.CreatedAt,
		Data: envEventFromDbModel(env),
	}))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to insert pending event messages")
	}
	return env, messages, nil
}

type DeleteEnvOptions struct {
	Force       bool
	DeleteRules *bool
}

func DeleteEnvironmentAndCreateMessage(
	ctx context.Context, db model.Databaser, tx model.Tx, orgId, projectId, envId string, opts DeleteEnvOptions,
) (*model.Environment, []*hstandardoutbox.PendingEventMessage, error) {
	env, err := db.GetEnvironment(ctx, tx, orgId, projectId, envId, model.GetModeForUpdate)
	if err != nil {
		return nil, nil, err
	}

	// Unless we use force mode: only continue when status is: active or delete_failed
	if !opts.Force {
		switch env.Status {
		case model.EnvironmentStatusActive:
		case model.EnvironmentStatusDeleteFailed:
		default:
			return nil, nil, model.NewErrConflict(fmt.Sprintf("cannot delete environment in status '%s'", env.Status))
		}
	}

	env, err = db.UpdateEnvironment(ctx, tx, orgId, projectId, envId, &model.EnvironmentPatch{
		UpdatedAt:     time.Now().UTC(),
		Status:        opt.Of(model.EnvironmentStatusDeleting),
		StatusMessage: opt.Of("Attempting to destroy environment"),
	})
	if err != nil {
		return nil, nil, err
	}

	// For asynchronous delete, we publish an updated event here. The CP worker, will then receive this event and coordinate
	// with the DP to execute a destroy deployment. Once the destroy deployment succeeds, the deployment completed event will
	// cause the env to be fully deleted from the worker process.
	event := envEventFromDbModel(env)
	event.Force = ref.Ref(opts.Force)
	event.DeleteRules = opts.DeleteRules

	messages, err := db.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentUpdated,
		Time: env.UpdatedAt,
		Data: event,
	}))
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	return env, messages, nil
}

// Create a new project environment
// (POST /orgs/{orgId}/apps/{projectId}/envs)
func (s *Server) CreateEnvironment(ctx context.Context, request CreateEnvironmentRequestObject) (CreateEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkProjectAuthorization(ctx, uid, request.OrgId, request.ProjectId, authz.PermissionEnvironmentWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
	ids.EnvId = request.Body.Id
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())
	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		proj, err := s.Database.GetProject(ctx, tx, request.OrgId, request.ProjectId, model.GetModeDefault)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return CreateEnvironment409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict(me.Error()))}, nil
			}
			return nil, errors.Wrap(err, "failed to get project")
		}

		et, err := s.Database.GetEnvironmentType(ctx, tx, request.OrgId, request.Body.EnvTypeId, model.GetModeDefault)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return CreateEnvironment409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict(me.Error()))}, nil
			}
			return nil, errors.Wrap(err, "failed to get environment type")
		}

		env, messages, err := s.createEnvAndMessageInDatabase(ctx, tx, proj, et, request.Body.Id, ref.DerefOr(request.Body.DisplayName, request.Body.Id))
		if err != nil {
			if me, ok := model.IsErrConflict(err); ok {
				return CreateEnvironment409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			}
			return nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}

		logger.Info("created environment", logging.ZapEnvTypeId(env.EnvTypeId), logging.ZapEnvUuid(env.Uuid))

		reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
		return CreateEnvironment201JSONResponse(envFromDbModel(env)), nil
	}
}

// Update a project environment
// (PATCH /orgs/{orgId}/apps/{projectId}/envs/{envId})
func (s *Server) UpdateEnvironment(ctx context.Context, request UpdateEnvironmentRequestObject) (UpdateEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkEnvAuthorization(ctx, uid, request.OrgId, request.ProjectId, request.EnvId, authz.PermissionEnvironmentWrite); err != nil {
		return nil, err
	}

	if request.Body == nil {
		// Nothing to update, return the current environment
		if out, err := s.Database.GetEnvironment(ctx, nil, request.OrgId, request.ProjectId, request.EnvId, model.GetModeDefault); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get environment")
		} else {
			return UpdateEnvironment200JSONResponse(envFromDbModel(out)), nil
		}
	}

	out, err := s.Database.UpdateEnvironment(ctx, nil, request.OrgId, request.ProjectId, request.EnvId, &model.EnvironmentPatch{
		DisplayName: opt.Of(request.Body.DisplayName),
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrapf(err, "failed to update environment %s/%s", request.ProjectId, request.EnvId)
	}
	return UpdateEnvironment200JSONResponse(envFromDbModel(out)), nil
}

// Get a project environment
// (GET /orgs/{orgId}/apps/{projectId}/envs/{envId})
func (s *Server) GetEnvironment(ctx context.Context, request GetEnvironmentRequestObject) (GetEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionEnvironmentRead); err != nil {
		return nil, err
	}
	if out, err := s.Database.GetEnvironment(ctx, nil, request.OrgId, request.ProjectId, request.EnvId, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get environment")
	} else {
		return GetEnvironment200JSONResponse(envFromDbModel(out)), nil
	}
}

// Delete a project environment
// (DELETE /orgs/{orgId}/apps/{projectId}/envs/{envId})
func (s *Server) DeleteEnvironment(ctx context.Context, request DeleteEnvironmentRequestObject) (DeleteEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkProjectAuthorization(ctx, uid, request.OrgId, request.ProjectId, authz.PermissionEnvironmentWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
	ids.EnvId = request.EnvId
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	env, messages, err := DeleteEnvironmentAndCreateMessage(ctx, s.Database, tx, request.OrgId, request.ProjectId, request.EnvId, DeleteEnvOptions{
		Force:       ref.DerefOr(request.Params.Force, false),
		DeleteRules: request.Params.DeleteRules,
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return DeleteEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		if me, ok := model.IsErrConflict(err); ok {
			return DeleteEnvironment409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	logger.Info("environment updated into deleting state")
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return DeleteEnvironment202JSONResponse(envFromDbModel(env)), nil
}

func (s *Server) InternalForceDeleteEnvironment(ctx context.Context, request InternalForceDeleteEnvironmentRequestObject) (InternalForceDeleteEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionEnvironmentWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
	ids.EnvId = request.EnvId
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	env, err := s.Database.GetEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId, model.GetModeForUpdate)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return InternalForceDeleteEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	}

	if request.Params.DeleteRules != nil && *request.Params.DeleteRules {
		if deletedRuleIds, err := s.Database.BulkDeleteModuleRuleDefinitions(ctx, tx, request.OrgId, model.DeleteModuleRulesParams{
			ByProjectId: &request.ProjectId,
			ByEnvId:     &request.EnvId,
		}); err != nil {
			return nil, errors.Wrap(err, "failed to bulk delete module rules for environment")
		} else {
			logger.Info("deleted module rules for environment", zap.Strings("deleted_rule_ids", deletedRuleIds))
		}
	}

	if err = s.Database.DeleteEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId); err != nil {
		return nil, err
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentDeleted,
		Time: time.Now().UTC(),
		Data: envEventFromDbModel(env),
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	logger.Info("environment deleted")
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return InternalForceDeleteEnvironment204Response{}, nil
}

func (s *Server) InternalUpdateEnvironment(ctx context.Context, request InternalUpdateEnvironmentRequestObject) (InternalUpdateEnvironmentResponseObject, error) {
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
	ids.EnvId = request.EnvId
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	var st opt.Opt[model.EnvironmentStatus]
	if request.Body.Status != nil {
		st = opt.Of(model.EnvironmentStatus(*request.Body.Status))
	}

	env, err := s.Database.UpdateEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId, &model.EnvironmentPatch{
		Status: st, StatusMessage: opt.OfRef(request.Body.StatusMessage), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return InternalUpdateEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		} else if me, ok := model.IsErrBadRequest(err); ok {
			return InternalUpdateEnvironment400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(me)}, nil
		}
		return nil, errors.Wrapf(err, "failed to update environment %s/%s", request.ProjectId, request.EnvId)
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorEnvironmentUpdated,
		Time: env.UpdatedAt,
		Data: envEventFromDbModel(env),
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	logger.Info("environment updated")
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return InternalUpdateEnvironment200JSONResponse(envFromDbModel(env)), nil
}

// Refresh the runner for a project environment
// (POST /orgs/{orgId}/projects/{projectId}/envs/{envId}/runner)
func (s *Server) UpdateRunnerInAnEnvironment(ctx context.Context, request UpdateRunnerInAnEnvironmentRequestObject) (UpdateRunnerInAnEnvironmentResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionRunnerWrite); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
	ids.EnvId = request.EnvId
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	env, err := s.Database.GetEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId, model.GetModeForUpdate)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateRunnerInAnEnvironment404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	}

	runners, err := s.getMatchingRunners(ctx, tx, request.OrgId, request.ProjectId, env.EnvTypeId)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get matching runners for environment %s/%s", request.ProjectId, request.EnvId)
	} else if len(runners) == 0 {
		return UpdateRunnerInAnEnvironment409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict("no matching runner found for the supplied environment"))}, nil
	}
	refreshedRunnerId := runners[0].Rule.RunnerId
	if ref.DerefOr(request.Params.DryRun, false) {
		logger.Info("dry run: returning runner ID without making changes", zap.String("runnerId", refreshedRunnerId))
		return UpdateRunnerInAnEnvironment200JSONResponse{RunnerId: refreshedRunnerId, Updated: false}, nil
	}

	if _, err := s.Database.UpdateEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId, &model.EnvironmentPatch{
		RunnerId: opt.Of(refreshedRunnerId), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, errors.Wrapf(err, "failed to refresh runner for environment %s/%s", request.ProjectId, request.EnvId)
	} else if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	} else {
		logger.Info("runner updated in environment", logging.ZapRunnerId(refreshedRunnerId))
		return UpdateRunnerInAnEnvironment200JSONResponse{RunnerId: refreshedRunnerId, Updated: true}, nil
	}
}

// Get an Environment by UUID
// (GET /internal/orgs/{orgId}/envs/{envUuid})
func (s *Server) GetInternalEnvironmentByUuid(ctx context.Context, request GetInternalEnvironmentByUuidRequestObject) (GetInternalEnvironmentByUuidResponseObject, error) {
	if out, err := s.Database.GetEnvironmentByUuid(ctx, nil, request.OrgId, request.EnvUuid, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetInternalEnvironmentByUuid404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get environment")
	} else {
		return GetInternalEnvironmentByUuid200JSONResponse(envFromDbModel(out)), nil
	}
}

// List Environments by project UUID
// (GET /internal/orgs/{orgId}/projects/{projectUuid}/envs)
func (s *Server) ListInternalEnvironmentsByProjectUuid(ctx context.Context, request ListInternalEnvironmentsByProjectUuidRequestObject) (ListInternalEnvironmentsByProjectUuidResponseObject, error) {
	page, next, err := s.Database.ListEnvironmentsByProjectUuid(
		ctx, nil, request.OrgId, request.ProjectUuid, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100),
	)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListInternalEnvironmentsByProjectUuid404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list environments")
	}
	out := make([]Environment, len(page))
	for i, item := range page {
		out[i] = envFromDbModel(&item)
	}
	return ListInternalEnvironmentsByProjectUuid200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}
