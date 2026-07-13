package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pkg/errors"

	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type Runner struct {
	OrgId                     string
	Id                        string
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	Description               opt.Opt[string]
	RunnerType                string
	RunnerConfiguration       map[string]interface{}
	RunnerConfigurationSecret *RunnerConfigurationSecret
	StateStorageType          string
	StateStorageConfiguration map[string]interface{}
}

type RunnerConfigurationSecret struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type RunnerPatch struct {
	UpdatedAt                 time.Time
	Description               opt.Opt[string]
	RunnerConfiguration       *map[string]interface{}
	RunnerConfigurationSecret *RunnerConfigurationSecret
	StateStorageType          opt.Opt[string]
	StateStorageConfiguration *map[string]interface{}
}

func (d *databaser) ListRunners(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []Runner, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, r.id, r.created_at, r.updated_at, r.type, r.description, r.configuration, r.state_type, r.state_configuration
         FROM org_check
		 LEFT JOIN runners r ON org_check.org_exists AND r.org_id = $1
		 WHERE id > $2
		 ORDER BY org_id, id LIMIT $3`,
		orgId, afterId, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to query rows")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]Runner, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := Runner{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.Id, &next.CreatedAt, &next.UpdatedAt, &next.RunnerType, opt.Scan(&next.Description), asJson(&next.RunnerConfiguration), &next.StateStorageType, asJson(&next.StateStorageConfiguration)); err != nil {
				return nil, "", fmt.Errorf("failed to scan row: %w", err)
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, last.Id, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) GetRunner(ctx context.Context, optionalTx Tx, orgId, id string) (*Runner, error) {
	res := Runner{
		OrgId: orgId,
		Id:    id,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT created_at, updated_at, type, description, configuration, config_secret, state_type, state_configuration
         FROM runners
		 WHERE org_id = $1 AND id = $2`,
		orgId, id,
	).Scan(&res.CreatedAt, &res.UpdatedAt, &res.RunnerType, opt.Scan(&res.Description), asJson(&res.RunnerConfiguration), asJson(&res.RunnerConfigurationSecret), &res.StateStorageType, asJson(&res.StateStorageConfiguration)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("runner not found")
		}
		return nil, err
	}
	return &res, nil
}

func (d *databaser) CreateRunner(ctx context.Context, tx Tx, request *Runner) (*Runner, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	ret := *request

	var runnerSecret *asJsonInner[RunnerConfigurationSecret]
	if request.RunnerConfigurationSecret != nil {
		runnerSecret = asJson(request.RunnerConfigurationSecret)
	}

	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO runners (org_id, id, created_at, updated_at, type, description, configuration, config_secret, state_type, state_configuration) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING created_at, updated_at`,
		request.OrgId, request.Id, request.CreatedAt, request.UpdatedAt, request.RunnerType, request.Description, asJson(&request.RunnerConfiguration), runnerSecret, request.StateStorageType, asJson(&request.StateStorageConfiguration),
	).Scan(&ret.CreatedAt, &ret.UpdatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Constraint == "runners_org_id_fkey" {
				return nil, NewErrConflict("organization not found")
			}
			if pqe.Constraint == "runners_pkey" {
				return nil, NewErrConflict(fmt.Sprintf("runner with id %s already exists", request.Id))
			}
		}
		return nil, err
	}

	return &ret, nil
}

func (d *databaser) DeleteRunner(ctx context.Context, optionalTx Tx, orgId, id string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM runners WHERE org_id = $1 AND id = $2`, orgId, id); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Constraint == "envs_org_id_runner_id_fkey" {
				return NewErrConflict("the runner is in use by an environment")
			}
		}
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("runner not found")
	}
	return nil
}

func (d *databaser) UpdateRunner(ctx context.Context, optionalTx Tx, orgId, id string, request *RunnerPatch) (*Runner, error) {
	var runnerConf *asJsonInner[map[string]interface{}]
	var stateConf *asJsonInner[map[string]interface{}]
	var runnerSecret *asJsonInner[RunnerConfigurationSecret]
	if request.RunnerConfiguration != nil {
		runnerConf = asJson(request.RunnerConfiguration)
	}
	if request.StateStorageConfiguration != nil {
		stateConf = asJson(request.StateStorageConfiguration)
	}
	if request.RunnerConfigurationSecret != nil {
		runnerSecret = asJson(request.RunnerConfigurationSecret)
	}
	var ret = Runner{
		OrgId: orgId,
		Id:    id,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `UPDATE runners 
			  SET description         = COALESCE($3, description),
			      configuration       = COALESCE($4, configuration),
				  config_secret  	  = COALESCE($5, config_secret),
			      state_type          = COALESCE($6, state_type),
			      state_configuration = COALESCE($7, state_configuration),
			      updated_at = $8
			  WHERE org_id = $1 AND id = $2
			  RETURNING created_at, updated_at, type, description, configuration, config_secret, state_type, state_configuration`,
		orgId, id, request.Description, runnerConf, runnerSecret, request.StateStorageType, stateConf, request.UpdatedAt,
	).Scan(&ret.CreatedAt, &ret.UpdatedAt, &ret.RunnerType, opt.Scan(&ret.Description), asJson(&ret.RunnerConfiguration), asJson(&ret.RunnerConfigurationSecret), &ret.StateStorageType, asJson(&ret.StateStorageConfiguration)); err != nil {
		return nil, errors.Wrap(err, "failed to update runner")
	} else if ret.RunnerType == "" {
		return nil, NewErrNotFound("runner not found")
	}
	return &ret, nil
}
