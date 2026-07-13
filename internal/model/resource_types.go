package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type ResourceType struct {
	OrgId                 opt.Opt[string]
	Id                    string
	Description           string
	OutputsSchema         map[string]interface{}
	CreatedAt             time.Time
	IsDeveloperAccessible bool
}

type ResourceTypePatch struct {
	Description           opt.Opt[string]
	OutputsSchema         *map[string]interface{}
	IsDeveloperAccessible opt.Opt[bool]
}

func (d *databaser) ListResourceTypes(ctx context.Context, optionalTx Tx, orgId *string, pageToken string, perPage int) ([]ResourceType, string, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT DISTINCT ON(id) org_id, id, "description", output_schema, created_at, is_developer_accessible
         FROM resource_types 
		 WHERE (org_id = $1 OR org_id IS NULL) AND id > $2
		 ORDER BY id, org_id NULLS LAST LIMIT $3`,
		opt.OfRef(orgId), afterId, limitPlusOne,
	)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to query resource_types")
	}

	defer func() {
		if err = rs.Close(); err != nil {
			logger.Error("failed to close row set", zap.Error(err))
		}
	}()
	out := make([]ResourceType, 0, limitPlusOne-1)
	for rs.Next() {
		next := ResourceType{}
		if err = rs.Scan(opt.Scan(&next.OrgId), &next.Id, &next.Description, asJson(&next.OutputsSchema), &next.CreatedAt, &next.IsDeveloperAccessible); err != nil {
			return nil, "", errors.Wrap(err, "failed to scan resource_types")
		}
		if len(out) >= limitPlusOne-1 {
			return out, out[len(out)-1].Id, nil
		}
		out = append(out, next)
	}
	if rs.Err() != nil {
		return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
	}
	nextPageToken := ""
	if len(out) > perPage {
		nextPageToken = out[len(out)-1].Id
	}
	return out, nextPageToken, nil
}

func (d *databaser) BulkGetResourceTypes(ctx context.Context, optionalTx Tx, orgId string, ids []string) (items []ResourceType, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT id org_id, id, "description", output_schema, created_at, is_developer_accessible
         FROM resource_types 
		 WHERE (org_id = $1 OR org_id IS NULL) AND id = ANY($2)`,
		orgId, pq.Array(ids),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query resource_types")
	}

	defer func() {
		if err = rs.Close(); err != nil {
			logger.Error("failed to close row set", zap.Error(err))
		}
	}()
	out := make([]ResourceType, 0)
	for rs.Next() {
		next := ResourceType{}
		if err = rs.Scan(opt.Scan(&next.OrgId), &next.Id, &next.Description, asJson(&next.OutputsSchema), &next.CreatedAt, &next.IsDeveloperAccessible); err != nil {
			return nil, errors.Wrap(err, "failed to scan resource_types")
		}
		out = append(out, next)
	}
	if rs.Err() != nil {
		return nil, errors.Wrap(rs.Err(), "failed to iterate rows")
	}
	return out, nil
}

func (d *databaser) GetResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string) (*ResourceType, error) {
	res := &ResourceType{
		OrgId: opt.OfRef(orgId),
		Id:    id,
	}
	row := d.txOrDb(optionalTx).QueryRowContext(ctx,
		`SELECT description, output_schema, created_at, is_developer_accessible FROM resource_types WHERE org_id = $1 AND id = $2`, orgId, id)
	if err := row.Scan(&res.Description, asJson(&res.OutputsSchema), &res.CreatedAt, &res.IsDeveloperAccessible); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("resource type not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return res, nil
}

func (d *databaser) CreateResourceType(ctx context.Context, optionalTx Tx, request *ResourceType) (*ResourceType, error) {
	ret := *request
	row := d.txOrDb(optionalTx).QueryRowContext(ctx,
		`INSERT INTO resource_types (org_id, id, "description", output_schema, created_at, is_developer_accessible) VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
		request.OrgId, request.Id, request.Description, asJson(&request.OutputsSchema), request.CreatedAt, request.IsDeveloperAccessible,
	)
	if err := row.Scan(&ret.CreatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if strings.Contains(pqe.Message, "resource_types_unq") {
				return nil, NewErrConflict("resource type already exists")
			}
		}
		return nil, errors.Wrap(err, "failed to insert row")
	}
	return &ret, nil
}

func (d *databaser) UpdateResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string, request *ResourceTypePatch) (*ResourceType, error) {
	var outputsSchema *asJsonInner[map[string]interface{}]
	if request.OutputsSchema != nil {
		outputsSchema = asJson(request.OutputsSchema)
	}

	row := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `UPDATE resource_types 
			  SET "description" = COALESCE($3, description),
			      output_schema = COALESCE($4, output_schema),
				  is_developer_accessible = COALESCE($5, is_developer_accessible)
			  WHERE (org_id IS NULL AND $1::text IS NULL OR org_id = $1::text) AND id = $2
			  RETURNING "description", output_schema, created_at, is_developer_accessible`,
		opt.OfRef(orgId), id, request.Description, outputsSchema, request.IsDeveloperAccessible.Ref(),
	)
	ret := ResourceType{
		OrgId: opt.OfRef(orgId),
		Id:    id,
	}
	if err := row.Scan(&ret.Description, asJson(&ret.OutputsSchema), &ret.CreatedAt, &ret.IsDeveloperAccessible); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("resource type not found")
		}
		return nil, errors.Wrap(err, "failed to update row")
	}
	return &ret, nil
}

func (d *databaser) DeleteResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string) error {
	if optionalTx == nil {
		return fmt.Errorf("optional transaction cannot be nil")
	}

	if orgId != nil {
		var count int
		if err := optionalTx.QueryRowContext(ctx, `SELECT 1 FROM definitions WHERE org_id = $1 AND resource_type = $2 LIMIT 1`, *orgId, id).Scan(&count); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(err, "failed to query modules")
			}
			// pass through
		} else if count > 0 {
			return NewErrConflict("modules are still using this resource type")
		}
	}

	if res, err := optionalTx.ExecContext(ctx,
		`DELETE FROM resource_types 
         WHERE (org_id IS NULL AND $1::text IS NULL OR org_id = $1::text) AND id = $2`,
		orgId, id); err != nil {
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("resource type not found")
	}
	return nil
}
