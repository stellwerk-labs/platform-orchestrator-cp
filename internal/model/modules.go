package model

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
)

type ModuleDefinitionVersion struct {
	OrgId        string
	DefinitionId string
	CreatedAt    time.Time
	ResourceType string

	VersionId        string
	UpdatedAt        time.Time
	Description      opt.Opt[string]
	ModuleSource     string
	ModuleSourceCode opt.Opt[string]
	ModuleInputs     map[string]interface{}
	ModuleParams     map[string]ModuleParam
	Dependencies     map[string]ModuleDefinitionDependency
	CoProvisioned    []ModuleDefinitionCoProvision
	ProviderMapping  map[string]string
}

type ModuleDefinitionDependency struct {
	Type   string                 `json:"type"`
	Class  opt.Opt[string]        `json:"class,omitempty"`
	Id     opt.Opt[string]        `json:"id,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type ModuleDefinitionCoProvision struct {
	Type                      string                 `json:"type"`
	Class                     opt.Opt[string]        `json:"class,omitempty"`
	Id                        opt.Opt[string]        `json:"id,omitempty"`
	Params                    map[string]interface{} `json:"params,omitempty"`
	IsDependentOnCurrent      bool                   `json:"is_dependent_on_current"`
	CopyDependentsFromCurrent bool                   `json:"copy_dependents_from_current"`
}

type ModuleParam struct {
	Type        string `json:"type"`
	IsOptional  bool   `json:"is_optional"`
	Description string `json:"description"`
}

type ListModuleDefinitionsParams struct {
	ByResourceType  *string
	ByDefinitionIds []string
}

type ListModuleDefinitionVersionsParams struct{}

func (d *databaser) ListModuleDefinitions(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleDefinitionsParams) (items []ModuleDefinitionVersion, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	afterId := ""
	if pageToken != "" {
		afterId = pageToken
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`WITH org_check AS (SELECT EXISTS (SELECT 1 FROM orgs WHERE id = $1) AS org_exists)
		 SELECT org_check.org_exists, d.id, d.created_at, d.resource_type, d.latest_version_id, v.created_at, v.description, v.module_source, v.module_source_code, v.module_inputs, v.module_params, v.dependencies, v.coprovisioned, v.provider_mapping
		 FROM org_check
         LEFT JOIN definitions d ON org_check.org_exists AND d.org_id = $1 
		 INNER JOIN definition_versions v ON d.org_id = v.org_id AND d.id = v.definition_id AND d.latest_version_id = v.version_id
		 WHERE d.org_id = $1 AND ($4::text IS NULL OR d.resource_type = $4) AND ($5::text[] IS NULL OR id = ANY($5)) AND d.id > $2
		 ORDER BY id LIMIT $3`,
		orgId, afterId, limitPlusOne, params.ByResourceType, pq.Array(params.ByDefinitionIds),
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to query rows")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]ModuleDefinitionVersion, 0, limitPlusOne-1)
		for rs.Next() {
			var orgExists bool
			next := ModuleDefinitionVersion{OrgId: orgId}
			if err := rs.Scan(&orgExists, &next.DefinitionId, &next.CreatedAt, &next.ResourceType, &next.VersionId, &next.UpdatedAt, opt.Scan(&next.Description), &next.ModuleSource, opt.Scan(&next.ModuleSourceCode), asJson(&next.ModuleInputs), asJson(&next.ModuleParams), asJson(&next.Dependencies), asJson(&next.CoProvisioned), asJson(&next.ProviderMapping)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if !orgExists {
				return nil, "", NewErrNotFound("organization not found")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, last.DefinitionId, nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) ListModuleDefinitionVersions(ctx context.Context, optionalTx Tx, orgId string, defId string, pageToken string, perPage int, _ ListModuleDefinitionVersionsParams) (items []ModuleDefinitionVersion, nextPageToken string, err error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)

	beforeUpdatedAt := time.Now().UTC()
	if pageToken != "" {
		i, err := strconv.ParseInt(pageToken, 10, 64)
		if err != nil {
			return nil, "", errors.Wrapf(err, "failed to parse page token %q", pageToken)
		}
		beforeUpdatedAt = time.Unix(0, i)
	}
	limitPlusOne := max(1, perPage) + 1

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT d.created_at, d.resource_type, v.version_id, v.created_at, v.description, v.module_source, v.module_source_code, v.module_inputs, v.module_params, v.dependencies, v.coprovisioned, v.provider_mapping
         FROM definitions d INNER JOIN definition_versions v ON d.org_id = v.org_id AND d.id = v.definition_id
		 WHERE d.org_id = $1 AND d.id = $2 AND v.created_at < $3
		 ORDER BY v.created_at DESC LIMIT $4`,
		orgId, defId, beforeUpdatedAt, limitPlusOne,
	); err != nil {
		return nil, "", errors.Wrap(err, "failed to query rows")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		out := make([]ModuleDefinitionVersion, 0, limitPlusOne-1)
		for rs.Next() {
			next := ModuleDefinitionVersion{OrgId: orgId, DefinitionId: defId}
			if err := rs.Scan(&next.CreatedAt, &next.ResourceType, &next.VersionId, &next.UpdatedAt, opt.Scan(&next.Description), &next.ModuleSource, opt.Scan(&next.ModuleSourceCode), asJson(&next.ModuleInputs), asJson(&next.ModuleParams), asJson(&next.Dependencies), asJson(&next.CoProvisioned), asJson(&next.ProviderMapping)); err != nil {
				return nil, "", errors.Wrap(err, "failed to scan row")
			}
			if len(out) >= limitPlusOne-1 {
				last := out[len(out)-1]
				return out, strconv.Itoa(int(last.UpdatedAt.UnixNano())), nil
			}
			out = append(out, next)
		}
		if rs.Err() != nil {
			return nil, "", errors.Wrap(rs.Err(), "failed to iterate rows")
		}
		return out, nextPageToken, nil
	}
}

func (d *databaser) GetModuleDefinition(ctx context.Context, optionalTx Tx, orgId, defId string, mode GetMode) (*ModuleDefinitionVersion, error) {
	res := ModuleDefinitionVersion{
		OrgId:        orgId,
		DefinitionId: defId,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT d.latest_version_id, d.created_at, d.resource_type, v.created_at, v.description, v.module_source, v.module_source_code, v.module_inputs, v.module_params, v.dependencies, v.coprovisioned, v.provider_mapping
FROM definitions d INNER JOIN definition_versions v ON d.org_id = v.org_id AND d.id = v.definition_id AND d.latest_version_id = v.version_id
WHERE d.org_id = $1 AND d.id = $2`+GetModeSuffix(mode),
		orgId, defId,
	).Scan(&res.VersionId, &res.CreatedAt, &res.ResourceType, &res.UpdatedAt, opt.Scan(&res.Description), &res.ModuleSource, opt.Scan(&res.ModuleSourceCode), asJson(&res.ModuleInputs), asJson(&res.ModuleParams), asJson(&res.Dependencies), asJson(&res.CoProvisioned), asJson(&res.ProviderMapping)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("module not found")
		}
		return nil, err
	}
	return &res, nil
}

func (d *databaser) GetModuleDefinitionVersion(ctx context.Context, optionalTx Tx, orgId, defId string, versionId string) (*ModuleDefinitionVersion, error) {
	res := ModuleDefinitionVersion{
		OrgId:        orgId,
		DefinitionId: defId,
		VersionId:    versionId,
	}
	if err := d.txOrDb(optionalTx).QueryRowContext(
		ctx, `SELECT d.created_at, d.resource_type, v.created_at, v.description, v.module_source, v.module_source_code, v.module_inputs, v.module_params, v.dependencies, v.coprovisioned, v.provider_mapping
FROM definitions d INNER JOIN definition_versions v ON d.org_id = v.org_id AND d.id = v.definition_id
WHERE d.org_id = $1 AND d.id = $2 AND v.version_id = $3`,
		orgId, defId, versionId,
	).Scan(&res.CreatedAt, &res.ResourceType, &res.UpdatedAt, opt.Scan(&res.Description), &res.ModuleSource, opt.Scan(&res.ModuleSourceCode), asJson(&res.ModuleInputs), asJson(&res.ModuleParams), asJson(&res.Dependencies), asJson(&res.CoProvisioned), asJson(&res.ProviderMapping)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("definition version not found")
		}
		return nil, err
	}
	return &res, nil
}

func (d *databaser) BulkGetModuleDefinitionVersions(ctx context.Context, optionalTx Tx, orgId string, defIds []string, versionIds []string) ([]ModuleDefinitionVersion, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(d.logger, ctx)
	out := make([]ModuleDefinitionVersion, 0, len(defIds))

	concatIds := make([]string, 0, len(defIds))
	for i, defId := range defIds {
		concatIds = append(concatIds, defId+versionIds[i])
	}

	if rs, err := d.txOrDb(optionalTx).QueryContext(
		ctx,
		`SELECT d.id, v.version_id, d.created_at, d.resource_type, v.created_at, v.description, v.module_source, v.module_source_code, v.module_inputs, v.module_params, v.dependencies, v.coprovisioned, v.provider_mapping
FROM definitions d INNER JOIN definition_versions v ON d.org_id = v.org_id AND d.id = v.definition_id
WHERE d.org_id = $1 AND d.id || v.version_id = ANY($2::text[])`,
		orgId, pq.Array(concatIds),
	); err != nil {
		return nil, errors.Wrap(err, "failed to query rows")
	} else {
		defer func() {
			if err := rs.Close(); err != nil {
				logger.Error("failed to close row set")
			}
		}()
		for rs.Next() {
			var item ModuleDefinitionVersion
			if err := rs.Scan(&item.DefinitionId, &item.VersionId, &item.CreatedAt, &item.ResourceType, &item.UpdatedAt, opt.Scan(&item.Description), &item.ModuleSource, opt.Scan(&item.ModuleSourceCode), asJson(&item.ModuleInputs), asJson(&item.ModuleParams), asJson(&item.Dependencies), asJson(&item.CoProvisioned), asJson(&item.ProviderMapping)); err != nil {
				return nil, errors.Wrap(err, "failed to scan row")
			}
			out = append(out, item)
		}
		if rs.Err() != nil {
			return nil, errors.Wrap(rs.Err(), "failed to iterate rows")
		}
	}

	return out, nil
}

var validIdentifierSyntax = regexp.MustCompile(`^[a-zA-Z_-][a-zA-Z0-9_-]+$`)

func validateProviders(ctx context.Context, tx Tx, orgId string, providerMapping map[string]string) (error, error) {
	pTypes, pIds := make([]string, 0, len(providerMapping)), make([]string, 0, len(providerMapping))
	for s, s2 := range providerMapping {
		if !validIdentifierSyntax.MatchString(s) {
			return fmt.Errorf("invalid provider mapping '%s => %s': %s is not a valid identifier", s, s2, s), nil
		}
		parts := strings.SplitN(s2, ".", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid provider mapping '%s => %s': %s must be provider <type>.<id>", s, s2, s2), nil
		}
		pTypes = append(pTypes, parts[0])
		pIds = append(pIds, parts[1])
	}
	var badType, badId string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT a, b FROM unnest($2::text[], $3::text[]) as x(a,b) WHERE NOT EXISTS (SELECT 1 FROM providers WHERE org_id = $1 AND provider_type = x.a AND id = x.b) LIMIT 1`,
		orgId, pq.Array(pTypes), pq.Array(pIds),
	).Scan(&badType, &badId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return fmt.Errorf("invalid provider mapping: no provider exists with type %s and id %s", badType, badId), nil
}

func providersToStableList(providerMapping map[string]string) []string {
	list := make([]string, 0, len(providerMapping))
	for _, p := range providerMapping {
		if !slices.Contains(list, p) {
			list = append(list, p)
		}
	}
	slices.Sort(list)
	return list
}

func (d *databaser) CreateModuleDefinition(ctx context.Context, tx Tx, request *ModuleDefinitionVersion) (*ModuleDefinitionVersion, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	ret := *request

	if userErr, err := validateProviders(ctx, tx, request.OrgId, request.ProviderMapping); err != nil {
		return nil, errors.Wrap(err, "error validating providers")
	} else if userErr != nil {
		return nil, NewErrBadRequest(userErr.Error())
	}

	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO definitions (org_id, id, created_at, resource_type, latest_version_id) VALUES ($1, $2, $3, $4, $5) RETURNING created_at`,
		request.OrgId, request.DefinitionId, request.CreatedAt, request.ResourceType, request.VersionId,
	).Scan(&ret.CreatedAt); err != nil {
		if pqe := new(pq.Error); errors.As(err, &pqe) {
			if pqe.Constraint == "definitions_org_id_fkey" {
				return nil, NewErrConflict("organization not found")
			}
			if pqe.Constraint == "definitions_pkey" {
				return nil, NewErrConflict(fmt.Sprintf("module definition with id %s already exists", request.DefinitionId))
			}
		}
		return nil, errors.Wrap(err, "failed to insert definition")
	}

	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO definition_versions (org_id, definition_id, version_id, created_at, description, module_source, module_source_code, module_inputs, module_params, dependencies, coprovisioned, provider_mapping, provider_values) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING created_at`,
		request.OrgId, request.DefinitionId, request.VersionId, request.UpdatedAt, request.Description, request.ModuleSource, request.ModuleSourceCode, asJson(&request.ModuleInputs), asJson(&request.ModuleParams), asJson(&request.Dependencies), asJson(&request.CoProvisioned), asJson(&request.ProviderMapping),
		pq.Array(providersToStableList(request.ProviderMapping)),
	).Scan(&ret.UpdatedAt); err != nil {
		return nil, errors.Wrap(err, "failed to insert definition version")
	}

	return &ret, nil
}

func (d *databaser) CreateModuleDefinitionVersion(ctx context.Context, tx Tx, request *ModuleDefinitionVersion) (*ModuleDefinitionVersion, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	ret := *request

	if userErr, err := validateProviders(ctx, tx, request.OrgId, request.ProviderMapping); err != nil {
		return nil, errors.Wrap(err, "error validating providers")
	} else if userErr != nil {
		return nil, NewErrBadRequest(userErr.Error())
	}

	if err := tx.QueryRowContext(
		ctx,
		`INSERT INTO definition_versions (org_id, definition_id, version_id, created_at, description, module_source, module_source_code, module_inputs, module_params, dependencies, coprovisioned, provider_mapping, provider_values) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING created_at`,
		request.OrgId, request.DefinitionId, request.VersionId, request.UpdatedAt, &request.Description, request.ModuleSource, request.ModuleSourceCode, asJson(&request.ModuleInputs), asJson(&request.ModuleParams), asJson(&request.Dependencies), asJson(&request.CoProvisioned), asJson(&request.ProviderMapping),
		pq.Array(providersToStableList(request.ProviderMapping)),
	).Scan(&ret.UpdatedAt); err != nil {
		return nil, err
	}

	if err := tx.QueryRowContext(
		ctx,
		`UPDATE definitions SET latest_version_id = $3 WHERE org_id = $1 AND id = $2 RETURNING created_at, resource_type`,
		request.OrgId, request.DefinitionId, request.VersionId,
	).Scan(&ret.CreatedAt, &ret.ResourceType); err != nil {
		return nil, err
	}

	return &ret, nil
}

func (d *databaser) DeleteModuleDefinitionVersion(ctx context.Context, optionalTx Tx, orgId, defId string, versionId string) error {
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM definition_versions WHERE org_id = $1 AND definition_id = $2 AND version_id = $3`, orgId, defId, versionId); err != nil {
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("definition version not found")
	}
	return nil
}

func (d *databaser) DeleteModuleDefinition(ctx context.Context, optionalTx Tx, orgId, defId string) error {
	// cascade delete should handle definition versions and the
	if res, err := d.txOrDb(optionalTx).ExecContext(ctx, `DELETE FROM definitions WHERE org_id = $1 AND id = $2`, orgId, defId); err != nil {
		return err
	} else if rc, _ := res.RowsAffected(); rc == 0 {
		return NewErrNotFound("module not found")
	}
	return nil
}
