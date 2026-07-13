package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type EnvType struct {
	OrgId       string
	OrgUuid     uuid.UUID
	Id          string
	Uuid        uuid.UUID
	DisplayName string
	CreatedAt   time.Time
}

type UpdateEnvTypeParams struct {
	DisplayName opt.Opt[string]
}

func (d *databaser) ListEnvironmentTypes(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []EnvType, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, e.org_uuid, e.id, e.uuid, e.display_name, e.created_at
		 FROM org_check
		 LEFT JOIN env_types e ON org_check.org_exists AND e.org_id = $1
		 WHERE e.id > $2 
		 ORDER BY e.id 
		 LIMIT $3`, orgId, afterId, limitPlusOne); err != nil {
		return nil, "", errors.Wrap(err, "failed to query env type rows")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]EnvType, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := EnvType{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.OrgUuid, &next.Id, &next.Uuid, &next.DisplayName, &next.CreatedAt); err != nil {
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

func (d *databaser) CreateEnvironmentType(ctx context.Context, optionalTx Tx, request *EnvType) (*EnvType, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx).With(logging.ZapOrgId(request.OrgId), logging.ZapEnvTypeId(request.Id))
	ret := *request
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `INSERT INTO env_types (org_id, org_uuid, id, display_name, created_at) VALUES ($1, $2, $3, $4, $5) RETURNING created_at, uuid`,
		request.OrgId, request.OrgUuid, request.Id, request.DisplayName, request.CreatedAt).Scan(&ret.CreatedAt, &ret.Uuid); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Code.Name() == UniqueViolationErrorCode {
				return nil, NewErrConflict("environment type already exists")
			}
			if pqe.Constraint == "env_types_org_id_fkey" {
				return nil, NewErrConflict("organization not found")
			}
		}
		return nil, errors.Wrap(err, "failed to insert environment type")
	}
	logger.Info("inserted new environment type")
	return &ret, nil
}

func (d *databaser) GetEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string, mode GetMode) (*EnvType, error) {
	ret := EnvType{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT org_uuid, id, uuid, display_name, created_at FROM env_types WHERE org_id = $1 AND id = $2`, orgId, id).Scan(&ret.OrgUuid, &ret.Id, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("environment type not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) UpdateEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string, params UpdateEnvTypeParams) (*EnvType, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := &EnvType{
		OrgId: orgId,
		Id:    id,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `UPDATE env_types
			  SET display_name = COALESCE($3, display_name)
			  WHERE org_id = $1 AND id = $2
			  RETURNING org_uuid, uuid, display_name, created_at`,
		orgId, id, params.DisplayName.Ref(),
	).Scan(&ret.OrgUuid, &ret.Uuid, &ret.DisplayName, &ret.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("environment type not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	logger.Info("updated environment type", logging.ZapOrgId(ret.OrgId), logging.ZapEnvTypeId(ret.Id))
	return ret, nil
}

func (d *databaser) DeleteEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string) error {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx).With(logging.ZapOrgId(orgId), logging.ZapEnvTypeId(id))

	var blockingProjectId, blockingEnvId string
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT project_id, id FROM envs WHERE org_id = $1 AND env_type_id = $2 LIMIT 1`, orgId, id).Scan(&blockingProjectId, &blockingEnvId); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return errors.Wrap(err, "failed to query row")
		}
	} else if blockingProjectId != "" {
		return NewErrConflict(fmt.Sprintf("environment type is in use by app '%s' env '%s'", blockingProjectId, blockingEnvId))
	}
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM env_types WHERE org_id = $1 AND id = $2`, orgId, id); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Constraint == "envs_org_id_env_type_id_fkey" {
				return NewErrConflict("environment type is in use")
			}
		}
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("environment type not found")
	}
	logger.Info("deleted environment type row")
	return nil
}
