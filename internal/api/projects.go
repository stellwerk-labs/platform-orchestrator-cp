package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/hmessaging/reliableoutbox"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

func projectFromDbProject(p *model.Project) Project {
	return Project{
		Id:          p.Id,
		Uuid:        p.Uuid,
		DisplayName: p.DisplayName,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
		Status:      ProjectStatus(p.Status),
	}
}

func projectEventFromDbModel(p *model.Project) genevents.ProjectChangedData {
	return genevents.ProjectChangedData{
		OrgId:       p.OrgId,
		OrgUuid:     p.OrgUuid,
		ProjectId:   p.Id,
		ProjectUuid: p.Uuid,
		Status:      ref.RefStringEmptyNil(string(p.Status)),
	}
}

// List projects
// (GET /orgs/{orgId}/projects)
func (s *Server) ListProjects(ctx context.Context, request ListProjectsRequestObject) (ListProjectsResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListProjects(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100))
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListProjects404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list project")
	}
	out := make([]Project, len(page))
	for i, item := range page {
		out[i] = projectFromDbProject(&item)
	}
	return ListProjects200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

// Create a new project
// (POST /orgs/{orgId}/projects)
func (s *Server) CreateProject(ctx context.Context, request CreateProjectRequestObject) (CreateProjectResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.Body.Id
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

	org, err := s.Database.GetOrg(ctx, tx, request.OrgId)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return CreateProject409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(model.NewErrConflict(me.Message))}, nil
		}
		return nil, errors.Wrap(err, "failed to get organization")
	}

	now := time.Now().UTC()
	project, err := s.Database.CreateProject(ctx, tx, &model.Project{
		OrgId: org.Id, OrgUuid: org.Uuid, Id: request.Body.Id, DisplayName: ref.DerefOr(request.Body.DisplayName, request.Body.Id),
		CreatedAt: now, UpdatedAt: now, Status: model.ProjectStatusActive,
	})
	if err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return CreateProject409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to create project")
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorProjectCreated,
		Time: project.CreatedAt,
		Data: projectEventFromDbModel(project),
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}
	logger.Info("created project", logging.ZapProjectUuid(project.Uuid))

	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return CreateProject201JSONResponse(projectFromDbProject(project)), nil
}

// Get a project
// (GET /orgs/{orgId}/projects/{projectId})
func (s *Server) GetProject(ctx context.Context, request GetProjectRequestObject) (GetProjectResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	if out, err := s.Database.GetProject(ctx, nil, request.OrgId, request.ProjectId, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetProject404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get project")
	} else {
		return GetProject200JSONResponse(projectFromDbProject(out)), nil
	}
}

// Update a project
// (PATCH /orgs/{orgId}/projects/{projectId})
func (s *Server) UpdateProject(ctx context.Context, request UpdateProjectRequestObject) (UpdateProjectResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkProjectAuthorization(ctx, authz.CanManageProjectCheck, uid, request.OrgId, request.ProjectId); err != nil {
		return nil, err
	}

	if request.Body == nil {
		if out, err := s.Database.GetProject(ctx, nil, request.OrgId, request.ProjectId, model.GetModeDefault); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateProject404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get project")
		} else {
			return UpdateProject200JSONResponse(projectFromDbProject(out)), nil
		}
	}

	out, err := s.Database.UpdatedProject(ctx, nil, request.OrgId, request.ProjectId, model.UpdateProjectParams{
		DisplayName: opt.Of(request.Body.DisplayName),
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return UpdateProject404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to update project")
	}
	return UpdateProject200JSONResponse(projectFromDbProject(out)), nil
}

// Delete a project
// (DELETE /orgs/{orgId}/projects/{projectId})
func (s *Server) DeleteProject(ctx context.Context, request DeleteProjectRequestObject) (DeleteProjectResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgManageAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.ProjectId = request.ProjectId
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

	proj, err := s.Database.GetProject(ctx, tx, request.OrgId, request.ProjectId, model.GetModeForUpdate)
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return DeleteProject404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get project")
	}

	if request.Params.DeleteRules != nil && *request.Params.DeleteRules {
		if deletedRuleIds, err := s.Database.BulkDeleteModuleRuleDefinitions(ctx, tx, request.OrgId, model.DeleteModuleRulesParams{
			ByProjectId: &request.ProjectId,
		}); err != nil {
			return nil, errors.Wrap(err, "failed to bulk delete module rules for project")
		} else {
			logger.Info("deleted module rules for project", zap.Strings("deleted_rule_ids", deletedRuleIds))
		}

		if deletedRunnerRuleIds, err := s.Database.BulkDeleteRunnerRules(ctx, tx, request.OrgId, model.DeleteRunnerRulesParams{
			ByProjectId: &request.ProjectId,
		}); err != nil {
			return nil, errors.Wrap(err, "failed to bulk delete runner rules for project")
		} else {
			logger.Info("deleted runner rules for project", zap.Strings("deleted_rule_ids", deletedRunnerRuleIds))
		}
	}

	if err := s.Database.DeleteProject(ctx, tx, request.OrgId, request.ProjectId); err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return DeleteProject409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, err
	}

	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorProjectDeleted,
		Time: time.Now().UTC(),
		Data: projectEventFromDbModel(proj),
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}
	logger.Info("project deleted")

	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return DeleteProject204Response{}, nil
}

// Get a Project by UUID
// (GET /internal/orgs/{orgId}/projects/{projectUuid})
func (s *Server) GetInternalProjectByUuid(ctx context.Context, request GetInternalProjectByUuidRequestObject) (GetInternalProjectByUuidResponseObject, error) {
	if out, err := s.Database.GetProjectByUuid(ctx, nil, request.OrgId, request.ProjectUuid, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetInternalProjectByUuid404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get project")
	} else {
		return GetInternalProjectByUuid200JSONResponse(projectFromDbProject(out)), nil
	}
}
