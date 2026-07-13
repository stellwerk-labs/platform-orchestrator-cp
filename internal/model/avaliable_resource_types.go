package model

import (
	"context"
	"database/sql"
	"time"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type AvailableResourceType struct {
	OrgId         opt.Opt[string]
	Id            string
	Description   string
	OutputsSchema map[string]interface{}
	CreatedAt     time.Time
	Options       []Option
}

type Option struct {
	ProjectId     *string                `json:"project_id"`
	EnvTypeId     *string                `json:"env_type_id"`
	EnvId         *string                `json:"env_id"`
	ResourceId    string                 `json:"resource_id"`
	ResourceClass string                 `json:"resource_class"`
	RuleId        string                 `json:"rule_id"`
	DefinitionId  string                 `json:"definition_id"`
	ModuleParams  map[string]ModuleParam `json:"module_params"`
}

type ListAvailableResourceTypeParams struct {
	TypeId                        opt.Opt[string]
	IncludeNonDeveloperAccessible opt.Opt[bool]
}

func (d *databaser) ListAvailableResourceTypes(ctx context.Context, optionalTx Tx, orgId, projectId, envId string, pageToken string, perPage int, filters ListAvailableResourceTypeParams) ([]AvailableResourceType, string, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	var envTypeId string
	err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT env_type_id FROM envs WHERE project_id = $1 AND id = $2`, projectId, envId).Scan(&envTypeId)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, "", errors.Wrapf(NewErrNotFound("environment not found"), "environment %s", envId)
		}
		return nil, "", errors.Wrapf(err, "failed to query environment %s", envId)
	}

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	query := `
		SELECT DISTINCT ON (rt.id)
			rt.id AS resource_type_id,
			rt.description AS resource_type_description,
			rt.output_schema,
			json_agg(
				json_build_object(
					'project_id', r.project_id,
					'env_id', r.env_id,
					'env_type_id', r.env_type_id,
					'resource_id', r.resource_id,
					'resource_class', r.resource_class,
					'rule_id', r.id,
					'definition_id', r.definition_id,
                    'module_params', dv.module_params
				)
			) AS option
		FROM resource_types AS rt
		LEFT JOIN definition_rules AS r ON (rt.org_id = r.org_id OR rt.org_id IS NULL) AND rt.id = r.resource_type
		LEFT JOIN definitions d ON d.org_id = r.org_id AND d.id = r.definition_id
		LEFT JOIN definition_versions dv ON dv.org_id = d.org_id AND dv.definition_id = d.id AND dv.version_id = d.latest_version_id 
		WHERE
			r.org_id = $3 AND 
			(r.project_id = $4 OR r.project_id IS NULL) AND 
			((r.project_id = $4 AND r.env_id = $5) OR r.env_id IS NULL) AND 
			(r.env_type_id = $6 OR r.env_type_id IS NULL) AND 
			(rt.is_developer_accessible = true OR $8 IS TRUE) AND
			(rt.id = $7 OR ($7 IS NULL AND rt.id > $1))
		GROUP BY
			rt.id, rt.description, rt.output_schema, rt.org_id, r.org_id
		ORDER BY
			rt.id,
			CASE 
				WHEN rt.org_id = r.org_id THEN 1
				ELSE 2
			END
		LIMIT $2`

	rs, err := d.txOrDb(optionalTx).QueryContext(ctx, query, afterId, limitPlusOne, orgId, projectId, envId, envTypeId, filters.TypeId.Ref(), filters.IncludeNonDeveloperAccessible.Ref())
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to query resource_types")
	}

	defer func() {
		if err = rs.Close(); err != nil {
			logger.Error("failed to close row set", zap.Error(err))
		}
	}()
	out := []AvailableResourceType{}
	for rs.Next() {
		resourceType := AvailableResourceType{}
		if err = rs.Scan(&resourceType.Id, &resourceType.Description, asJson(&resourceType.OutputsSchema), asJson(&resourceType.Options)); err != nil {
			return nil, "", errors.Wrap(err, "failed to scan resource_types")
		}
		if len(out) >= limitPlusOne-1 {
			return out, out[len(out)-1].Id, nil
		}
		out = append(out, resourceType)
	}
	if rs.Err() != nil {
		return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
	}
	return out, "", nil
}
