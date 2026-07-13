package model

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type Project struct {
	Id          string
	Uuid        uuid.UUID
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	OrgId       string
	OrgUuid     uuid.UUID
	Status      ProjectStatus
}

type ProjectStatus string

const (
	ProjectStatusActive   ProjectStatus = "active"
	ProjectStatusDeleting ProjectStatus = "deleting"
)

type UpdateProjectParams struct {
	DisplayName opt.Opt[string]
	Status      opt.Opt[ProjectStatus]
	UpdatedAt   time.Time
}

func (d *databaser) ListProjects(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []Project, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, p.org_uuid, p.id, p.uuid, p.display_name, p.created_at, p.updated_at, p.status
		 FROM org_check
		 LEFT JOIN projects p ON org_check.org_exists AND p.org_id = $1
		 WHERE p.id > $2 
		 ORDER BY p.id 
		 LIMIT $3`,
		orgId, afterId, limitPlusOne); err != nil {
		return nil, "", errors.Wrap(err, "failed to list projects")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Project, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := Project{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.OrgUuid, &next.Id, &next.Uuid, &next.DisplayName, &next.CreatedAt, &next.UpdatedAt, &next.Status); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
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

func (d *databaser) GetProject(ctx context.Context, optionalTx Tx, orgId, id string, mode GetMode) (*Project, error) {
	ret := Project{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx,
		`SELECT id, uuid, org_uuid, display_name, created_at, updated_at, status FROM projects WHERE org_id = $1 AND id = $2`+GetModeSuffix(mode),
		orgId, id,
	).Scan(&ret.Id, &ret.Uuid, &ret.OrgUuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, &ret.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("project not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) GetProjectByUuid(ctx context.Context, optionalTx Tx, orgId string, uuid uuid.UUID, mode GetMode) (*Project, error) {
	ret := Project{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx,
		`SELECT id, uuid, org_uuid, display_name, created_at, updated_at, status FROM projects WHERE org_id = $1 AND uuid = $2`+GetModeSuffix(mode),
		orgId, uuid,
	).Scan(&ret.Id, &ret.Uuid, &ret.OrgUuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, &ret.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("project not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) CreateProject(ctx context.Context, optionalTx Tx, request *Project) (*Project, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := *request
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx,
		`INSERT INTO projects(id, display_name, org_id, org_uuid, created_at, updated_at, status) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING uuid, created_at, updated_at`,
		request.Id, request.DisplayName, request.OrgId, request.OrgUuid, request.CreatedAt, request.UpdatedAt, request.Status,
	).Scan(&ret.Uuid, &ret.CreatedAt, &ret.UpdatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Code.Name() == UniqueViolationErrorCode {
				return nil, NewErrConflict("project already exists")
			}
			if pqe.Constraint == "projects_org_id_fkey" {
				return nil, NewErrConflict("organization not found")
			}
		}
		return nil, errors.Wrap(err, "failed to insert project")
	}
	logger.Info("inserted new project", logging.ZapOrgId(ret.OrgId), logging.ZapProjectId(ret.Id), logging.ZapProjectUuid(ret.Uuid))
	return &ret, nil
}

func (d *databaser) UpdatedProject(ctx context.Context, optional Tx, orgId, id string, params UpdateProjectParams) (*Project, error) {
	ret := Project{Id: id, OrgId: orgId}
	if err := d.txOrDb(optional).QueryRowContext(
		ctx,
		`UPDATE projects SET display_name = COALESCE($3, display_name), status = COALESCE($4, status), updated_at = $5 WHERE org_id = $1 AND id = $2 RETURNING org_uuid, uuid, display_name, created_at, updated_at, status`,
		orgId, id, params.DisplayName, params.Status, params.UpdatedAt,
	).Scan(&ret.OrgUuid, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt, &ret.UpdatedAt, &ret.Status); err != nil {
		return nil, errors.Wrap(err, "failed to update project")
	}
	return &ret, nil
}

func (d *databaser) DeleteProject(ctx context.Context, optionalTx Tx, orgId, id string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM projects WHERE org_id = $1 AND id = $2`, orgId, id); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Constraint == "envs_org_id_project_id_fkey" || pqe.Constraint == "envs_project_uuid_fkey" {
				return NewErrConflict("project contains environments")
			}
		}
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("project not found")
	}
	return nil
}
