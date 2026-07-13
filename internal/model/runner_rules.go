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

type ListRunnerRulesParams struct {
	ByRunnerId *string

	EffectiveInProjectId *string
	EffectiveInEnvTypeId *string
}

type DeleteRunnerRulesParams struct {
	ByRunnerId  *string
	ByProjectId *string
}

type RunnerRule struct {
	OrgId     string
	RunnerId  string
	CreatedAt time.Time
	Id        uuid.UUID
	ProjectId opt.Opt[string]
	EnvTypeId opt.Opt[string]
}

var pageTokenCodec = pagination.PageTokenCodec{Parts: 2}

func (d *databaser) ListRunnerRules(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListRunnerRulesParams) (items []RunnerRule, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	after0, after1 := "", uuid.Nil
	if parts, parseErr := pageTokenCodec.Parse(pageToken); parseErr != nil {
		return nil, "", NewErrBadRequest(parseErr.Error())
	} else if pageToken != "" {
		after0 = parts[0]
		if after1, err = uuid.Parse(parts[1]); err != nil {
			return nil, "", NewErrBadRequest("failed to parse uuid from page token")
		}
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, r.id, r.created_at, r.runner_id, r.project_id, r.env_type_id
		 FROM org_check
		 LEFT JOIN runner_rules r ON org_check.org_exists AND r.org_id = $1
		 WHERE
		 	( $2::text IS NULL OR r.runner_id = $2) AND
			( $3::text IS NULL OR r.project_id IS NULL OR r.project_id = $3 ) AND
			( $4::text IS NULL OR r.env_type_id IS NULL OR r.env_type_id = $4 ) AND

			-- pagination filter
        	(r.runner_id, r.id) > ($5, $6)
			ORDER BY r.org_id, r.runner_id, r.id LIMIT $7`,
		orgId, params.ByRunnerId, params.EffectiveInProjectId, params.EffectiveInEnvTypeId, after0, after1, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to query runner rules")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set", zap.Error(err))
			}
		}()
		out := make([]RunnerRule, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := RunnerRule{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.Id, &next.CreatedAt, &next.RunnerId, opt.Scan(&next.ProjectId), opt.Scan(&next.EnvTypeId)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, pageTokenCodec.Generate(last.RunnerId, last.Id.String()), nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(err, "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) CreateRunnerRule(ctx context.Context, tx Tx, orgId string, request *RunnerRule) (*RunnerRule, error) {
	if tx == nil {
		return nil, errors.New("tx is nil")
	} else if request.RunnerId == "" {
		// these should be filled in at the api layer
		return nil, errors.New("runnerId is empty")
	}

	// conflict detection
	var conflictId string
	var conflictRunnerId string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT id, runner_id FROM runner_rules WHERE org_id = $1 AND (project_id, env_type_id) IS NOT DISTINCT FROM ($2, $3) FOR UPDATE`,
		orgId, request.ProjectId.Ref(), request.EnvTypeId.Ref(),
	).Scan(&conflictId, &conflictRunnerId); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.Wrap(err, "failed to query runner rules")
		}
	} else {
		return nil, NewErrConflict(fmt.Sprintf("runner rule conflicts with existing rule '%s' for runner '%s'", conflictId, conflictRunnerId))
	}

	ret := *request
	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO runner_rules (org_id, created_at, runner_id, env_type_id, project_id) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		orgId, request.CreatedAt, request.RunnerId, request.EnvTypeId, request.ProjectId,
	).Scan(&ret.Id, &ret.CreatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			switch pqe.Constraint {
			case "runner_rules_org_id_runner_id_fkey":
				return nil, NewErrConflict(fmt.Sprintf("runner '%s' not found", request.RunnerId))
			case "runner_rules_org_id_env_type_id_fkey":
				return nil, NewErrConflict(fmt.Sprintf("environment type '%s' not found", request.EnvTypeId.Must()))
			case "runner_rules_org_id_project_id_fkey":
				return nil, NewErrConflict(fmt.Sprintf("project '%s' not found", request.ProjectId.Must()))
			}
		}
		return nil, errors.Wrap(err, "failed to insert runner rule")
	}

	return &ret, nil
}

func (d *databaser) GetRunnerRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) (*RunnerRule, error) {
	res := RunnerRule{OrgId: orgId}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT id, created_at, runner_id, project_id, env_type_id FROM runner_rules WHERE org_id = $1 AND id = $2`, orgId, ruleId,
	).Scan(&res.Id, &res.CreatedAt, &res.RunnerId, opt.Scan(&res.ProjectId), opt.Scan(&res.EnvTypeId)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("runner rule not found")
		}
		return nil, errors.Wrap(err, "failed to query row")
	}
	return &res, nil
}

func (d *databaser) DeleteRunnerRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(
		ctx, `DELETE FROM runner_rules WHERE org_id = $1 AND id = $2`, orgId, ruleId,
	); err != nil {
		return errors.Wrap(err, "failed to delete row")
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("runner rule not found")
	}
	return nil
}

func (d *databaser) BulkDeleteRunnerRules(ctx context.Context, tx Tx, orgId string, params DeleteRunnerRulesParams) ([]string, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	if rs, err := d.txOrDb(tx).QueryContext(
		ctx,
		`DELETE FROM runner_rules r
		WHERE r.org_id = $1 AND

		-- user filter
		($2::text IS NULL OR r.project_id = $2) AND
		($3::text IS NULL OR r.runner_id = $3)

		RETURNING r.id`,
		orgId, params.ByProjectId, params.ByRunnerId,
	); err != nil {
		return nil, errors.Wrap(err, "failed to bulk delete runner rules")
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
