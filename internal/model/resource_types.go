package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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
	CatalogueStatus       string
	ResourceVersion       int64
	ArchivedAt            *time.Time
	ArchivedBy            *uuid.UUID
	ArchiveReason         string
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
		`SELECT DISTINCT ON(id) org_id, id, "description", output_schema, created_at, is_developer_accessible,
			catalogue_status, resource_version, archived_at, archived_by, COALESCE(archive_reason, '')
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
		if err = scanResourceType(rs, &next); err != nil {
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
		`SELECT org_id, id, "description", output_schema, created_at, is_developer_accessible,
			catalogue_status, resource_version, archived_at, archived_by, COALESCE(archive_reason, '')
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
		if err = scanResourceType(rs, &next); err != nil {
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
		`SELECT org_id, id, description, output_schema, created_at, is_developer_accessible,
			catalogue_status, resource_version, archived_at, archived_by, COALESCE(archive_reason, '')
		 FROM resource_types WHERE (org_id = $1 OR org_id IS NULL) AND id = $2
		 ORDER BY org_id NULLS LAST LIMIT 1`, orgId, id)
	if err := scanResourceType(row, res); err != nil {
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
		`INSERT INTO resource_types (org_id, id, "description", output_schema, created_at, is_developer_accessible) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING created_at, catalogue_status, resource_version`,
		request.OrgId, request.Id, request.Description, asJson(&request.OutputsSchema), request.CreatedAt, request.IsDeveloperAccessible,
	)
	if err := row.Scan(&ret.CreatedAt, &ret.CatalogueStatus, &ret.ResourceVersion); err != nil {
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
	if _, err := d.GetResourceType(ctx, optionalTx, orgId, id); err != nil {
		return nil, err
	}
	return nil, NewErrConflict("resource types are immutable; create a new resource type identity for a changed contract")
}

func scanResourceType(row interface{ Scan(...any) error }, result *ResourceType) error {
	var archivedAt sql.NullTime
	var archivedBy uuid.NullUUID
	if err := row.Scan(opt.Scan(&result.OrgId), &result.Id, &result.Description, asJson(&result.OutputsSchema),
		&result.CreatedAt, &result.IsDeveloperAccessible, &result.CatalogueStatus, &result.ResourceVersion,
		&archivedAt, &archivedBy, &result.ArchiveReason); err != nil {
		return err
	}
	if archivedAt.Valid {
		result.ArchivedAt = &archivedAt.Time
	}
	if archivedBy.Valid {
		value := archivedBy.UUID
		result.ArchivedBy = &value
	}
	return nil
}

func (d *databaser) SetResourceTypeCatalogueStatus(ctx context.Context, tx Tx, orgID, id, target string, actor uuid.UUID, reason string, expectedVersion int64) (*ResourceType, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, NewErrBadRequest("reason is required")
	}
	if target != catalogueStatusActive && target != catalogueStatusArchived {
		return nil, NewErrBadRequest("invalid resource type catalogue status")
	}
	current, err := d.GetResourceType(ctx, tx, &orgID, id)
	if err != nil {
		return nil, err
	}
	if !current.OrgId.IsSet() || current.OrgId.Must() != orgID {
		return nil, NewErrConflict("built-in resource types cannot be managed through an organization")
	}
	if current.ResourceVersion != expectedVersion {
		return nil, NewErrConflict("resource type changed since it was read")
	}
	if current.CatalogueStatus == target {
		return current, nil
	}
	var row *sql.Row
	if target == "archived" {
		row = tx.QueryRowContext(ctx, `UPDATE resource_types SET catalogue_status = 'archived', archived_at = now(), archived_by = $3,
			archive_reason = $4, resource_version = resource_version + 1 WHERE org_id = $1 AND id = $2
			RETURNING org_id, id, description, output_schema, created_at, is_developer_accessible,
			catalogue_status, resource_version, archived_at, archived_by, COALESCE(archive_reason, '')`, orgID, id, actor, reason)
	} else {
		row = tx.QueryRowContext(ctx, `UPDATE resource_types SET catalogue_status = 'active', archived_at = NULL, archived_by = NULL,
			archive_reason = NULL, resource_version = resource_version + 1 WHERE org_id = $1 AND id = $2
			RETURNING org_id, id, description, output_schema, created_at, is_developer_accessible,
			catalogue_status, resource_version, archived_at, archived_by, COALESCE(archive_reason, '')`, orgID, id)
	}
	updated := &ResourceType{}
	if err := scanResourceType(row, updated); err != nil {
		return nil, errors.Wrap(err, "failed to change resource type catalogue status")
	}
	eventType := "resource_type.archived"
	if target == catalogueStatusActive {
		eventType = "resource_type.unarchived"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO resource_type_catalogue_events
		(org_id, resource_type_id, event_type, resource_version, actor, reason) VALUES ($1, $2, $3, $4, $5, $6)`,
		orgID, id, eventType, updated.ResourceVersion, actor, reason); err != nil {
		return nil, errors.Wrap(err, "failed to append resource type catalogue event")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.resource-type.catalogue.changed", map[string]any{
		eventFieldOrgID: orgID, "resource_type": id, "catalogue_status": target,
		eventFieldResourceVer: updated.ResourceVersion, eventFieldActor: actor, eventFieldReason: reason,
	}); err != nil {
		return nil, err
	}
	return updated, nil
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

		if err := optionalTx.QueryRowContext(ctx, `SELECT 1 FROM resource_type_catalogue_events WHERE org_id = $1 AND resource_type_id = $2 LIMIT 1`, *orgId, id).Scan(&count); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return errors.Wrap(err, "failed to query resource type catalogue history")
			}
		} else {
			return NewErrConflict("resource types with catalogue history cannot be deleted or reused")
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
