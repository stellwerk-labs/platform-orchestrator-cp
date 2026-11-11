package model

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pagination"
)

type ModuleProvider struct {
	OrgId               string
	ProviderType        string
	Id                  string
	CreatedAt           time.Time
	Description         opt.Opt[string]
	Source              string
	VersionConstraint   string
	DeletionRequestedAt opt.Opt[time.Time]
	Config              map[string]interface{}
}

type ListModuleProvidersParams struct {
	ByType        *string
	ByProviderIds []string
}

var providersPageTokenCodec = pagination.PageTokenCodec{Parts: 2}

func (d *databaser) ListModuleProviders(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleProvidersParams) (items []ModuleProvider, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterType, afterId := "", ""
	if parts, parseErr := providersPageTokenCodec.Parse(pageToken); parseErr != nil {
		return nil, "", NewErrBadRequest(parseErr.Error())
	} else if pageToken != "" {
		afterType, afterId = parts[0], parts[1]
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, p.provider_type, p.id, p.description, p.created_at, p.source, p.version_constraint, p.configuration, p.deletion_requested_at
		 FROM org_check
		 LEFT JOIN providers p ON org_check.org_exists AND p.org_id = $1
		 WHERE (p.provider_type > $2 OR (p.provider_type = $2 AND p.id > $3)) AND ($4::text IS NULL OR p.provider_type = $4) AND ($6::text[] IS NULL OR concat(p.provider_type, '.', id) = ANY($6))
		 ORDER BY p.provider_type, p.id LIMIT $5`,
		orgId, afterType, afterId, params.ByType, limitPlusOne, pq.Array(params.ByProviderIds),
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to list providers")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]ModuleProvider, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := ModuleProvider{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.ProviderType, &next.Id, opt.Scan(&next.Description), &next.CreatedAt, &next.Source, &next.VersionConstraint, asJson(&next.Config), opt.Scan(&next.DeletionRequestedAt)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, providersPageTokenCodec.Generate(last.ProviderType, last.Id), nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) CreateModuleProvider(ctx context.Context, optionalTx Tx, request *ModuleProvider) (*ModuleProvider, error) {
	ret := *request
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `INSERT INTO providers (org_id, provider_type, id, description, created_at, source, version_constraint, configuration) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) returning created_at`,
		request.OrgId, request.ProviderType, request.Id, request.Description, request.CreatedAt, request.Source, request.VersionConstraint, asJson(&request.Config),
	).Scan(&ret.CreatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if strings.Contains(pqe.Message, "providers_pk") {
				return nil, NewErrConflict("provider already exists")
			}
		}
		return nil, errors.Wrap(err, "failed to insert provider")
	}
	return &ret, nil
}

func (d *databaser) GetModuleProvider(ctx context.Context, optionalTx Tx, orgId, providerType, id string) (*ModuleProvider, error) {
	res := &ModuleProvider{
		OrgId:        orgId,
		ProviderType: providerType,
		Id:           id,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT description, created_at, source, version_constraint, configuration, deletion_requested_at FROM providers WHERE org_id = $1 AND provider_type = $2 AND id = $3`, orgId, providerType, id).
		Scan(opt.Scan(&res.Description), &res.CreatedAt, &res.Source, &res.VersionConstraint, asJson(&res.Config), opt.Scan(&res.DeletionRequestedAt)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("provider not found")
		}
		return nil, errors.Wrap(err, "failed to query provider")
	}
	return res, nil
}

func (d *databaser) DeleteModuleProvider(ctx context.Context, tx Tx, orgId, providerType, id string) error {
	if err := lockProviderLifecycle(ctx, tx, orgId); err != nil {
		return err
	}
	provider, err := d.GetModuleProvider(ctx, tx, orgId, providerType, id)
	if err != nil {
		return err
	}
	if provider.DeletionRequestedAt.IsSet() {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE providers SET deletion_requested_at=clock_timestamp() WHERE org_id=$1 AND provider_type=$2 AND id=$3`, orgId, providerType, id); err != nil {
		return err
	}
	return advanceProviderLifecycleEpoch(ctx, tx, orgId)
}

func (d *databaser) UpdateModuleProvider(ctx context.Context, optionalTx Tx, request *ModuleProvider) (*ModuleProvider, error) {
	ret := *request
	if res, err := d.txOrDb(optionalTx).ExecContext(
		ctx, `UPDATE providers SET description = $4, version_constraint = $5, configuration = $6 WHERE org_id = $1 AND provider_type = $2 AND id = $3`,
		request.OrgId, request.ProviderType, request.Id, request.Description, request.VersionConstraint, asJson(&request.Config),
	); err != nil {
		return nil, errors.Wrap(err, "failed to update provider")
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return nil, NewErrNotFound("provider not found")
	}
	return &ret, nil
}
