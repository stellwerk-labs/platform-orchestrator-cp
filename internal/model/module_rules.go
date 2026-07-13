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
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pagination"
)

type ListModuleRulesParams struct {
	ByDefinitionId *string
	ByResourceType *string
	ByProjectId    *string
	ByEnvId        *string

	EffectiveInProjectId *string
	EffectiveInEnvTypeId *string
	EffectiveInEnvId     *string
}

type DeleteModuleRulesParams struct {
	ByModuleId  *string
	ByProjectId *string
	ByEnvId     *string
}

type DefinitionRule struct {
	OrgId         string
	DefinitionId  string
	CreatedAt     time.Time
	Id            uuid.UUID
	ResourceType  string
	ResourceClass string
	ResourceId    opt.Opt[string]
	ProjectId     opt.Opt[string]
	EnvTypeId     opt.Opt[string]
	EnvId         opt.Opt[string]
}

var moduleRulesPageTokenCodec = pagination.PageTokenCodec{Parts: 4}

func (d *databaser) ListModuleRules(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleRulesParams) (items []DefinitionRule, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	after0, after1, after2, after3 := "", "", "", uuid.Nil
	if parts, parseErr := moduleRulesPageTokenCodec.Parse(pageToken); parseErr != nil {
		return nil, "", NewErrBadRequest(parseErr.Error())
	} else if pageToken != "" {
		after0, after1, after2 = parts[0], parts[1], parts[2]
		if after3, err = uuid.Parse(parts[3]); err != nil {
			return nil, "", NewErrBadRequest("failed to parse uuid from page token")
		}
	}
	limitPlusOne := max(1, perPage) + 1

	// The most 'useful' native sorting we can output here is a method to
	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, r.id, r.created_at, r.definition_id, r.resource_type, r.resource_class, r.resource_id, r.project_id, r.env_type_id, r.env_id
		 FROM org_check
		 LEFT JOIN definition_rules r ON org_check.org_exists AND r.org_id = $1
		 WHERE
			-- user filter
			($2::text IS NULL OR r.definition_id = $2) AND
			($3::text IS NULL OR r.resource_type = $3) AND
			($4::text IS NULL OR r.project_id = $4) AND
			($5::text IS NULL OR (r.project_id = $4 AND r.env_id = $5)) AND

			-- environment filters
			-- either rule doesn't specify an project, or the query doesn't filter by the project, or it matches
			( $10::text IS NULL OR r.project_id IS NULL OR r.project_id = $10 ) AND
			( $11::text IS NULL OR r.env_type_id IS NULL OR r.env_type_id = $11 ) AND
			( $12::text IS NULL OR r.env_id IS NULL OR (r.project_id = $10 AND r.env_id = $12) ) AND

			-- pagination filter
        	(r.resource_type, r.definition_id, r.resource_class, r.id) > ($6, $7, $8, $9)
			ORDER BY r.org_id, r.resource_type, r.definition_id, r.resource_class, r.id LIMIT $13`,
		orgId, params.ByDefinitionId, params.ByResourceType, params.ByProjectId, params.ByEnvId, after0, after1, after2, after3, params.EffectiveInProjectId, params.EffectiveInEnvTypeId, params.EffectiveInEnvId, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to query definition rules")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set", zap.Error(err))
			}
		}()
		out := make([]DefinitionRule, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := DefinitionRule{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.Id, &next.CreatedAt, &next.DefinitionId, &next.ResourceType, &next.ResourceClass, opt.Scan(&next.ResourceId), opt.Scan(&next.ProjectId), opt.Scan(&next.EnvTypeId), opt.Scan(&next.EnvId)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, moduleRulesPageTokenCodec.Generate(last.ResourceType, last.DefinitionId, last.ResourceClass, last.Id.String()), nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) CreateModuleRule(ctx context.Context, tx Tx, orgId string, request *DefinitionRule) (*DefinitionRule, error) {
	if tx == nil {
		return nil, errors.New("tx is nil")
	} else if request.ResourceType == "" || request.DefinitionId == "" {
		// these should be filled in at the api layer
		return nil, errors.New("resourceType or definitionId is empty")
	}

	// conflict detection
	var conflictId string
	var conflictDefinition string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT id, definition_id FROM definition_rules WHERE org_id = $1 AND resource_type = $2 AND resource_class = $3
		  AND (resource_id, env_type_id, project_id, env_id) IS NOT DISTINCT FROM ($4, $5, $6, $7) FOR UPDATE`,
		orgId, request.ResourceType, request.ResourceClass, request.ResourceId.Ref(), request.EnvTypeId.Ref(), request.ProjectId.Ref(), request.EnvId.Ref(),
	).Scan(&conflictId, &conflictDefinition); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.Wrap(err, "failed to query definition rules")
		}
	} else {
		return nil, NewErrConflict(fmt.Sprintf("rule conflicts with existing rule '%s' for definition '%s'", conflictId, conflictDefinition))
	}

	ret := *request
	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO definition_rules (org_id, created_at, definition_id, resource_type, resource_class, resource_id, env_type_id, project_id, env_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at`,
		orgId, request.CreatedAt, request.DefinitionId, request.ResourceType, request.ResourceClass, request.ResourceId, request.EnvTypeId, request.ProjectId, request.EnvId,
	).Scan(&ret.Id, &ret.CreatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			switch pqe.Constraint {
			case "definition_rules_org_id_env_type_id_fkey":
				return nil, NewErrConflict("environment type not found")
			case "definition_rules_org_id_project_id_fkey":
				return nil, NewErrConflict("project not found")
			case "definition_rules_org_id_project_id_env_id_fkey":
				return nil, NewErrConflict("environment not found")
			}
		}
		return nil, errors.Wrap(err, "failed to insert definition rule")
	}

	return &ret, nil
}

func (d *databaser) GetModuleRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) (*DefinitionRule, error) {
	res := DefinitionRule{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT id, created_at, definition_id, resource_type, resource_class, resource_id, project_id, env_type_id, env_id FROM definition_rules WHERE org_id = $1 AND id = $2`, orgId, ruleId,
	).Scan(&res.Id, &res.CreatedAt, &res.DefinitionId, &res.ResourceType, &res.ResourceClass, opt.Scan(&res.ResourceId), opt.Scan(&res.ProjectId), opt.Scan(&res.EnvTypeId), opt.Scan(&res.EnvId)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("module rule not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &res, nil
}

func (d *databaser) DeleteModuleRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(
		ctx, `DELETE FROM definition_rules WHERE org_id = $1 AND id = $2`, orgId, ruleId,
	); err != nil {
		return errors.Wrap(err, "failed to delete row")
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("module rule not found")
	}
	return nil
}

func (d *databaser) BulkDeleteModuleRuleDefinitions(ctx context.Context, tx Tx, orgId string, params DeleteModuleRulesParams) ([]string, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	if rs, err := d.txOrDb(tx).QueryContext(
		ctx,
		`DELETE FROM definition_rules r
		WHERE r.org_id = $1 AND

		-- user filter
		($2::text IS NULL OR r.project_id = $2) AND
		($3::text IS NULL OR (r.project_id = $2 AND r.env_id = $3))

		RETURNING r.id`,
		orgId, params.ByProjectId, params.ByEnvId,
	); err != nil {
		return nil, errors.Wrap(err, "failed to bulk delete definition rules")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set", zap.Error(err))
			}
		}()

		var deletedIds []string
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				return nil, errors.Wrap(err, "failed to scan deleted rule id")
			}
			deletedIds = append(deletedIds, id)
		}
		if rs.Err() != nil {
			return nil, errors.Wrap(err, "failed to iterate deleted rule ids")
		}
		return deletedIds, nil
	}
}
