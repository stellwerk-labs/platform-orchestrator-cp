package model

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/pkg/errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type Environment struct {
	Id            string
	ProjectId     string
	ProjectUuid   uuid.UUID
	EnvTypeId     string
	EnvTypeUuid   uuid.UUID
	Uuid          uuid.UUID
	DisplayName   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	OrgId         string
	OrgUuid       uuid.UUID
	RunnerId      opt.Opt[string]
	Status        EnvironmentStatus
	StatusMessage opt.Opt[string]
}

type EnvironmentStatus string

const (
	EnvironmentStatusActive       EnvironmentStatus = "active"
	EnvironmentStatusDeleting     EnvironmentStatus = "deleting"
	EnvironmentStatusDeleteFailed EnvironmentStatus = "delete_failed"
)

var allowedStatuses = []EnvironmentStatus{
	EnvironmentStatusActive,
	EnvironmentStatusDeleting,
	EnvironmentStatusDeleteFailed,
}

type EnvironmentPatch struct {
	RunnerId      opt.Opt[string]
	Status        opt.Opt[EnvironmentStatus]
	StatusMessage opt.Opt[string]
	DisplayName   opt.Opt[string]
	UpdatedAt     time.Time
}

type ListEnvironmentsParams struct {
	ByEnvTypeIds []string
}

func (d *databaser) ListEnvironments(ctx context.Context, optionalTx Tx, orgId, projectId string, pageToken string, perPage int, params ListEnvironmentsParams) (items []Environment, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists), project_check AS (SELECT EXISTS (SELECT 1 FROM projects WHERE id = $2) AS project_exists)
		 SELECT org_check.org_exists, project_check.project_exists, e.org_uuid, e.id, e.env_type_id, e.env_type_uuid, e.project_uuid, e.uuid, e.display_name, e.created_at, e.updated_at,e.runner_id, e.status, e.status_message
		 FROM org_check
		 CROSS JOIN project_check
		 LEFT JOIN envs e ON org_check.org_exists AND e.org_id = $1 AND e.project_id = $2 AND e.id > $3 AND ($5::text[] IS NULL OR e.env_type_id = ANY($5))
		 ORDER BY e.id
		 LIMIT $4`,
		orgId, projectId, afterId, limitPlusOne, pq.Array(params.ByEnvTypeIds),
	); err != nil {
		return nil, "", errors.Wrapf(err, "failed to list environments in project %s", projectId)
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Environment, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists, projExists bool
			var envId, envTypeId, displayName, status sql.NullString
			var orgUuid, envTypeUuid, projectUuid, envUuid uuid.NullUUID
			var createdAt, updatedAt sql.NullTime
			var runnerId, statusMessage opt.Opt[string]
			if err := rs.Scan(&orgExists, &projExists, &orgUuid, &envId, &envTypeId, &envTypeUuid, &projectUuid, &envUuid, &displayName, &createdAt, &updatedAt, opt.Scan(&runnerId), &status, opt.Scan(&statusMessage)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if !projExists {
				return nil, "", NewErrNotFound(fmt.Sprintf("project %s not found", projectId))
			}
			if !envId.Valid {
				continue
			}
			next := Environment{
				OrgId:         orgId,
				ProjectId:     projectId,
				OrgUuid:       orgUuid.UUID,
				Id:            envId.String,
				EnvTypeId:     envTypeId.String,
				EnvTypeUuid:   envTypeUuid.UUID,
				ProjectUuid:   projectUuid.UUID,
				Uuid:          envUuid.UUID,
				DisplayName:   displayName.String,
				CreatedAt:     createdAt.Time,
				UpdatedAt:     updatedAt.Time,
				RunnerId:      runnerId,
				Status:        EnvironmentStatus(status.String),
				StatusMessage: statusMessage,
			}
			if len(out) >= limitPlusOne-1 {
				return out, out[len(out)-1].Id, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) ListEnvironmentsInOrg(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListEnvironmentsParams) (items []Environment, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, e.org_uuid, e.id, e.env_type_id, e.env_type_uuid, e.project_id, e.project_uuid, e.uuid, e.display_name, e.created_at, e.updated_at,e.runner_id, e.status, e.status_message
		 FROM org_check
		 LEFT JOIN envs e ON org_check.org_exists AND e.org_id = $1 AND e.id > $2 AND ($4::text[] IS NULL OR e.env_type_id = ANY($4))
		 ORDER BY e.id
		 LIMIT $3`,
		orgId, afterId, limitPlusOne, pq.Array(params.ByEnvTypeIds),
	); err != nil {
		return nil, "", errors.Wrapf(err, "failed to list environments in org %s", orgId)
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Environment, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			var envId, envTypeId, displayName, status, projectId sql.NullString
			var orgUuid, envTypeUuid, projectUuid, envUuid uuid.NullUUID
			var createdAt, updatedAt sql.NullTime
			var runnerId, statusMessage opt.Opt[string]
			if err := rs.Scan(&orgExists, &orgUuid, &envId, &envTypeId, &envTypeUuid, &projectId, &projectUuid, &envUuid, &displayName, &createdAt, &updatedAt, opt.Scan(&runnerId), &status, opt.Scan(&statusMessage)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if !envId.Valid {
				continue
			}
			next := Environment{
				OrgId:         orgId,
				ProjectId:     projectId.String,
				OrgUuid:       orgUuid.UUID,
				Id:            envId.String,
				EnvTypeId:     envTypeId.String,
				EnvTypeUuid:   envTypeUuid.UUID,
				ProjectUuid:   projectUuid.UUID,
				Uuid:          envUuid.UUID,
				DisplayName:   displayName.String,
				CreatedAt:     createdAt.Time,
				UpdatedAt:     updatedAt.Time,
				RunnerId:      runnerId,
				Status:        EnvironmentStatus(status.String),
				StatusMessage: statusMessage,
			}
			if len(out) >= limitPlusOne-1 {
				return out, out[len(out)-1].Id, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) ListEnvironmentsByRunnerId(ctx context.Context, optionalTx Tx, orgId, runnerId string, pageToken string, perPage int) (items []Environment, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT e.org_uuid, e.id, e.env_type_id, e.env_type_uuid, e.uuid, e.display_name, e.created_at, e.updated_at, e.project_id, e.project_uuid, e.runner_id, e.status, e.status_message
		 FROM envs e
		 WHERE e.org_id = $1 AND e.runner_id = $2 AND e.id > $3
		 ORDER BY e.id 
		 LIMIT $4`,
		orgId, runnerId, afterId, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrapf(err, "failed to list environments for runner %s", runnerId)
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Environment, 0, limitPlusOne-1)
		for rs.Next() {
			next := Environment{OrgId: orgId, RunnerId: opt.Of(runnerId)}
			if err := rs.Scan(&next.OrgUuid, &next.Id, &next.EnvTypeId, &next.EnvTypeUuid, &next.Uuid, &next.DisplayName, &next.CreatedAt, &next.UpdatedAt, &next.ProjectId, &next.ProjectUuid, opt.Scan(&next.RunnerId), &next.Status, opt.Scan(&next.StatusMessage)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if len(out) >= limitPlusOne-1 {
				return out, out[len(out)-1].Id, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) GetEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string, mode GetMode) (*Environment, error) {
	ret := Environment{OrgId: orgId, ProjectId: projectId}
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT org_uuid, id, env_type_id, env_type_uuid, project_uuid, uuid, display_name, created_at, updated_at, runner_id, status, status_message FROM envs WHERE org_id = $1 AND project_id = $2 AND id = $3`, orgId, projectId, id).
		Scan(&ret.OrgUuid, &ret.Id, &ret.EnvTypeId, &ret.EnvTypeUuid, &ret.ProjectUuid, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, opt.Scan(&ret.RunnerId), &ret.Status, opt.Scan(&ret.StatusMessage)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("environment not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) GetEnvironmentByUuid(ctx context.Context, optionalTx Tx, orgId string, uuid uuid.UUID, mode GetMode) (*Environment, error) {
	ret := Environment{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT org_uuid, project_id, project_uuid, id, env_type_id, env_type_uuid, uuid, display_name, created_at, updated_at, runner_id, status, status_message FROM envs WHERE org_id = $1 AND uuid = $2`+GetModeSuffix(mode), orgId, uuid).
		Scan(&ret.OrgUuid, &ret.ProjectId, &ret.ProjectUuid, &ret.Id, &ret.EnvTypeId, &ret.EnvTypeUuid, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, opt.Scan(&ret.RunnerId), &ret.Status, opt.Scan(&ret.StatusMessage)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("environment not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) CreateEnvironment(ctx context.Context, optionalTx Tx, request *Environment) (*Environment, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := *request
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `INSERT INTO envs(id, project_id, project_uuid, env_type_id, env_type_uuid, display_name, org_id, org_uuid, created_at, updated_at, runner_id, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING uuid, created_at, updated_at, status`,
		request.Id, request.ProjectId, request.ProjectUuid, request.EnvTypeId, request.EnvTypeUuid, request.DisplayName, request.OrgId, request.OrgUuid, request.CreatedAt, request.UpdatedAt, request.RunnerId.Ref(), request.Status).Scan(&ret.Uuid, &ret.CreatedAt, &ret.UpdatedAt, &ret.Status); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Code.Name() == UniqueViolationErrorCode {
				return nil, NewErrConflict("environment already exists")
			} else if pqe.Constraint == "envs_org_id_fkey" {
				return nil, NewErrConflict("organization not found")
			} else if pqe.Constraint == "envs_org_id_project_id_fkey" {
				return nil, NewErrConflict(fmt.Sprintf("project %s/%s not found", request.OrgId, request.ProjectId))
			} else if pqe.Constraint == "envs_org_id_env_type_id_fkey" {
				return nil, NewErrConflict(fmt.Sprintf("environment type '%s' not found", request.EnvTypeId))
			}
		}
		return nil, errors.Wrap(err, "failed to insert environment")
	}
	logger.Info("inserted new environment", logging.ZapOrgId(ret.OrgId), logging.ZapProjectId(ret.ProjectId), logging.ZapEnvTypeId(ret.EnvTypeId), logging.ZapEnvId(ret.Id), logging.ZapEnvUuid(ret.Uuid))
	return &ret, nil
}

func (d *databaser) DeleteEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM envs WHERE org_id = $1 AND project_id = $2 and id = $3`, orgId, projectId, id); err != nil {
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("environment not found")
	}
	return nil
}

func (d *databaser) UpdateEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string, request *EnvironmentPatch) (*Environment, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := &Environment{
		OrgId:     orgId,
		ProjectId: projectId,
		Id:        id,
	}
	if s := request.Status.Ref(); s != nil && !slices.Contains(allowedStatuses, *s) {
		return nil, NewErrBadRequest(fmt.Sprintf("unknown status '%s'", *s))
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `UPDATE envs
			  SET runner_id = COALESCE($4, runner_id), 
			      status = COALESCE($5, status),
				  display_name = COALESCE($8, display_name),
			      -- Set the status message if not empty, otherwise clear it if the status has changed, otherwise leave it as is.
			      status_message = CASE WHEN $6::text IS NOT NULL THEN $6 WHEN $5::text IS NULL THEN status_message ELSE '' END, 
			      updated_at = $7
			  WHERE org_id = $1 AND project_id = $2 AND id = $3
			  RETURNING org_uuid, env_type_id, env_type_uuid, project_uuid, uuid, display_name, created_at, updated_at, runner_id, status, status_message`,
		orgId, projectId, id, request.RunnerId.Ref(), request.Status.Ref(), request.StatusMessage.Ref(), request.UpdatedAt, request.DisplayName.Ref(),
	).Scan(&ret.OrgUuid, &ret.EnvTypeId, &ret.EnvTypeUuid, &ret.ProjectUuid, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, opt.Scan(&ret.RunnerId), &ret.Status, opt.Scan(&ret.StatusMessage)); err != nil {
		return nil, errors.Wrap(err, "failed to update environment")
	} else if ret.UpdatedAt.IsZero() {
		return nil, NewErrNotFound("environment not found")
	}
	logger.Info("updated environment", logging.ZapOrgId(ret.OrgId), logging.ZapProjectId(ret.ProjectId), logging.ZapEnvTypeId(ret.EnvTypeId), logging.ZapEnvId(ret.Id), logging.ZapEnvUuid(ret.Uuid))
	return ret, nil
}

func (d *databaser) ListEnvironmentsByProjectUuid(ctx context.Context, optionalTx Tx, orgId string, projectUuid uuid.UUID, pageToken string, perPage int) (items []Environment, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists), project_check AS (SELECT EXISTS (SELECT 1 FROM projects WHERE org_id = $1 AND uuid = $2) AS project_exists)
		 SELECT org_check.org_exists, project_check.project_exists, e.org_uuid, e.id, e.env_type_id, e.env_type_uuid, e.project_id, e.project_uuid, e.uuid, e.display_name, e.created_at, e.updated_at,e.runner_id, e.status, e.status_message
		 FROM org_check
		 CROSS JOIN project_check
		 LEFT JOIN envs e ON org_check.org_exists AND e.org_id = $1 AND e.project_uuid = $2 AND e.id > $3
		 ORDER BY e.id
		 LIMIT $4`,
		orgId, projectUuid, afterId, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrapf(err, "failed to list environments in project %s", projectUuid)
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Environment, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists, projExists bool
			var envId, envTypeId, projectId, displayName, status sql.NullString
			var orgUuid, envTypeUuid, projUuid, envUuid uuid.NullUUID
			var createdAt, updatedAt sql.NullTime
			var runnerId, statusMessage opt.Opt[string]
			if err := rs.Scan(&orgExists, &projExists, &orgUuid, &envId, &envTypeId, &envTypeUuid, &projectId, &projUuid, &envUuid, &displayName, &createdAt, &updatedAt, opt.Scan(&runnerId), &status, opt.Scan(&statusMessage)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if !projExists {
				return nil, "", NewErrNotFound(fmt.Sprintf("project %s not found", projectUuid))
			}
			if !envId.Valid {
				continue
			}
			next := Environment{
				OrgId:         orgId,
				ProjectId:     projectId.String,
				ProjectUuid:   projectUuid,
				OrgUuid:       orgUuid.UUID,
				Id:            envId.String,
				EnvTypeId:     envTypeId.String,
				EnvTypeUuid:   envTypeUuid.UUID,
				Uuid:          envUuid.UUID,
				DisplayName:   displayName.String,
				CreatedAt:     createdAt.Time,
				UpdatedAt:     updatedAt.Time,
				RunnerId:      runnerId,
				Status:        EnvironmentStatus(status.String),
				StatusMessage: statusMessage,
			}
			if len(out) >= limitPlusOne-1 {
				return out, out[len(out)-1].Id, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}
