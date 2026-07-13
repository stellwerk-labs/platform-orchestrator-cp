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

type OrgSource string
type OrgPlan string

type OrgStatus string

const (
	OrgSourceInternal OrgSource = "internal"
	OrgSourcePublic   OrgSource = "public"

	OrgPlanCustom OrgPlan = "custom"

	OrgStatusActive       OrgStatus = "active"
	OrgStatusDeleting     OrgStatus = "deleting"
	OrgStatusDeleteFailed OrgStatus = "delete_failed"
	OrgStatusDeleted      OrgStatus = "deleted"
)

type Org struct {
	Id            string
	Uuid          uuid.UUID
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     opt.Opt[time.Time]
	Status        OrgStatus
	StatusMessage opt.Opt[string]
	Source        OrgSource
	Plan          OrgPlan
}

type ListOrgsParams struct {
	ByCreatedBy opt.Opt[string]
}

func (d *databaser) ListOrgs(ctx context.Context, optionalTx Tx, pageToken string, perPage int, params ListOrgsParams) (items []Org, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT id, uuid, created_at, created_by, updated_at, status, status_message, source, plan FROM orgs
WHERE id > $1
	AND ($3::uuid IS NULL OR created_by = $3)
ORDER BY id LIMIT $2`,
		afterId, limitPlusOne, params.ByCreatedBy,
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to list internal orgs")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Org, 0, limitPlusOne-1)
		for rs.Next() {
			next := Org{}
			if err := rs.Scan(&next.Id, &next.Uuid, &next.CreatedAt, &next.CreatedBy, opt.Scan(&next.UpdatedAt), &next.Status, opt.Scan(&next.StatusMessage), &next.Source, &next.Plan); err != nil {
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

func (d *databaser) GetOrg(ctx context.Context, optionalTx Tx, id string) (*Org, error) {
	ret := Org{}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT id, uuid, created_at, created_by, updated_at, status, status_message, source, plan FROM orgs WHERE id = $1`, id,
	).Scan(&ret.Id, &ret.Uuid, &ret.CreatedAt, &ret.CreatedBy, opt.Scan(&ret.UpdatedAt), &ret.Status, opt.Scan(&ret.StatusMessage), &ret.Source, &ret.Plan); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("organization not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &ret, nil
}

func (d *databaser) CreateOrg(ctx context.Context, optionalTx Tx, request *Org) (*Org, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := *request
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `INSERT INTO orgs(id, created_at, created_by, source, plan) VALUES ($1, $2, $3, $4, $5) RETURNING uuid, created_at, status`, request.Id, request.CreatedAt, request.CreatedBy, request.Source, request.Plan).Scan(&ret.Uuid, &ret.CreatedAt, &ret.Status); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Code.Name() == UniqueViolationErrorCode {
				return nil, NewErrConflict("organization already exists")
			}
		}
		return nil, errors.Wrap(err, "failed to insert internal org")
	}
	logger.Info("inserted new internal org", logging.ZapOrgId(ret.Id), logging.ZapOrgUuid(ret.Uuid))
	return &ret, nil
}

func (d *databaser) DeleteOrg(ctx context.Context, optionalTx Tx, id string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM orgs WHERE id = $1`, id); err != nil {
		return errors.Wrap(err, "failed to delete organization")
	} else if rowsAffected, _ := res.RowsAffected(); rowsAffected == 0 {
		return NewErrNotFound("organization not found")
	}
	return nil
}

type UpdateOrgRequest struct {
	CreatedBy     opt.Opt[uuid.UUID]
	Plan          opt.Opt[OrgPlan]
	UpdatedAt     opt.Opt[time.Time]
	Status        opt.Opt[OrgStatus]
	StatusMessage opt.Opt[string]
}

func (d *databaser) UpdateOrg(ctx context.Context, optionalTx Tx, id string, request UpdateOrgRequest) (*Org, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	ret := Org{Id: id}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx,
		`UPDATE orgs
SET created_by = COALESCE($2, created_by), plan = COALESCE($3, plan), updated_at = COALESCE($4, updated_at), status = COALESCE($5, status),
    status_message = CASE WHEN $6::text IS NOT NULL THEN $6 WHEN $5::text IS NULL THEN status_message ELSE '' END
WHERE id = $1
RETURNING uuid, created_at, created_by, updated_at, status, status_message, source, plan
`,
		id, request.CreatedBy.Ref(), request.Plan.Ref(), request.UpdatedAt.Ref(), request.Status.Ref(), request.StatusMessage.Ref(),
	).Scan(&ret.Uuid, &ret.CreatedAt, &ret.CreatedBy, opt.Scan(&ret.UpdatedAt), &ret.Status, opt.Scan(&ret.StatusMessage), &ret.Source, &ret.Plan); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("organization not found")
		}
		return nil, errors.Wrap(err, "failed to update organization")
	}
	logger.Info("updated organization", logging.ZapOrgId(ret.Id), logging.ZapOrgUuid(ret.Uuid))
	return &ret, nil
}
