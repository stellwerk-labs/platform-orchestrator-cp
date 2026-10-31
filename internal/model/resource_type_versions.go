package model

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleconformance"
)

type ResourceTypeVersion struct {
	UUID             uuid.UUID
	ResourceTypeUUID uuid.UUID
	ResourceTypeID   string
	Version          int64
	OutputSchema     map[string]any
	ModuleContract   map[string]any
	CreatedAt        time.Time
	CreatedBy        *uuid.UUID
	Reason           *string
}

type ResourceTypeVersionPublication struct {
	Version                 int64
	ExpectedResourceVersion int64
	OutputSchema            map[string]any
	ModuleContract          map[string]any
	Actor                   uuid.UUID
	Reason                  string
}

func scanResourceTypeVersion(row interface{ Scan(...any) error }) (*ResourceTypeVersion, error) {
	var v ResourceTypeVersion
	err := row.Scan(&v.UUID, &v.ResourceTypeUUID, &v.ResourceTypeID, &v.Version,
		asJson(&v.OutputSchema), asJson(&v.ModuleContract), &v.CreatedAt, &v.CreatedBy, &v.Reason)
	return &v, err
}

const resourceTypeVersionColumns = `v.uuid, v.resource_type_uuid, r.id, v.version,
	v.output_schema, v.module_contract, v.created_at, v.created_by, v.reason`

func (d *databaser) GetResourceTypeVersion(ctx context.Context, tx Tx, orgID, id string, version int64) (*ResourceTypeVersion, error) {
	v, err := scanResourceTypeVersion(d.txOrDb(tx).QueryRowContext(ctx, `SELECT `+resourceTypeVersionColumns+`
		FROM resource_type_versions v JOIN resource_types r ON r.uuid=v.resource_type_uuid
		WHERE r.uuid=(SELECT uuid FROM resource_types WHERE id=$2 AND (org_id=$1 OR org_id IS NULL)
		 ORDER BY org_id NULLS LAST LIMIT 1) AND v.version=$3`, orgID, id, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("Resource Type version not found")
	}
	return v, errors.Wrap(err, "failed to read Resource Type version")
}

func (d *databaser) ListResourceTypeVersions(ctx context.Context, tx Tx, orgID, id string, after int64, limit int) ([]ResourceTypeVersion, error) {
	if after < 0 || limit < 1 || limit > 101 {
		return nil, NewErrBadRequest("invalid Resource Type version page")
	}
	rows, err := d.txOrDb(tx).QueryContext(ctx, `SELECT `+resourceTypeVersionColumns+`
		FROM resource_type_versions v JOIN resource_types r ON r.uuid=v.resource_type_uuid
		WHERE r.uuid=(SELECT uuid FROM resource_types WHERE id=$2 AND (org_id=$1 OR org_id IS NULL)
		 ORDER BY org_id NULLS LAST LIMIT 1) AND v.version>$3 ORDER BY v.version LIMIT $4`, orgID, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]ResourceTypeVersion, 0)
	for rows.Next() {
		v, err := scanResourceTypeVersion(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *v)
	}
	return result, rows.Err()
}

func (d *databaser) PublishResourceTypeVersion(ctx context.Context, tx Tx, orgID, id string, request ResourceTypeVersionPublication) (*ResourceTypeVersion, error) {
	expected, output, contract, actor, reason := request.ExpectedResourceVersion, request.OutputSchema, request.ModuleContract, request.Actor, request.Reason
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if request.Version < 2 {
		return nil, NewErrBadRequest("publication requires an explicit interface version of at least 2; version 1 belongs to the catalogue identity")
	}
	if actor == uuid.Nil || strings.TrimSpace(reason) == "" || len(reason) > 2000 || output == nil {
		return nil, NewErrBadRequest("actor, non-empty reason and output_schema are required")
	}
	if err := moduleconformance.ValidateContract(ctx, contract); err != nil {
		return nil, conformanceBadRequest(err)
	}
	if err := lockResourceTypeIdentity(ctx, tx, id); err != nil {
		return nil, err
	}
	var resourceUUID uuid.UUID
	var revision int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT uuid,resource_version,catalogue_status FROM resource_types
		WHERE org_id=$1 AND id=$2 FOR UPDATE`, orgID, id).Scan(&resourceUUID, &revision, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("organization-owned Resource Type not found")
	}
	if err != nil {
		return nil, err
	}
	if revision != expected {
		return nil, NewErrConflict("Resource Type changed since it was read")
	}
	if status != catalogueStatusActive {
		return nil, NewErrConflict("archived Resource Types do not accept new contracts")
	}
	var number int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM resource_type_versions WHERE resource_type_uuid=$1`, resourceUUID).Scan(&number)
	if err != nil {
		return nil, err
	}
	if request.Version != number {
		return nil, NewErrConflict("the requested interface version must be the next unused version number")
	}
	var versionUUID uuid.UUID
	err = tx.QueryRowContext(ctx, `INSERT INTO resource_type_versions
		(resource_type_uuid,version,output_schema,module_contract,created_at,created_by,reason)
		VALUES ($1,$2,$3,$4,now(),$5,$6) RETURNING uuid`, resourceUUID, number, asJson(&output), asJson(&contract), actor, strings.TrimSpace(reason)).Scan(&versionUUID)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE resource_types SET resource_version=resource_version+1 WHERE uuid=$1`, resourceUUID); err != nil {
		return nil, err
	}
	if err = d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.resource-type.version.published", map[string]any{
		eventFieldOrgID: orgID, "resource_type_id": id, "resource_type_uuid": resourceUUID, eventFieldVersionUUID: versionUUID,
		"version": number, eventFieldResourceVer: revision + 1, eventFieldActor: actor, eventFieldReason: strings.TrimSpace(reason),
	}); err != nil {
		return nil, err
	}
	return d.GetResourceTypeVersion(ctx, tx, orgID, id, number)
}

// resolveModuleResourceTypeContract freezes the selected interface before any
// publication writes. Existing callers omit the version and keep version 1.
func (d *databaser) resolveModuleResourceTypeContract(ctx context.Context, tx Tx, definition *ModuleDefinitionVersion) (*ResourceType, error) {
	if definition.ResourceTypeVersion < 0 {
		return nil, NewErrBadRequest("resource_type_version must be positive")
	}
	version := definition.ResourceTypeVersion
	if version == 0 {
		version = 1
	}
	var result ResourceType
	var versionUUID uuid.UUID
	var number int64
	err := d.txOrDb(tx).QueryRowContext(ctx, `SELECT v.uuid, v.version, v.output_schema, v.module_contract
		FROM resource_type_versions v WHERE v.resource_type_uuid = COALESCE(
		  (SELECT resource_type_uuid FROM definitions WHERE org_id=$1 AND id=$2),
		  (SELECT uuid FROM resource_types WHERE id=$3 AND (org_id=$1 OR org_id IS NULL)
		   ORDER BY org_id NULLS LAST LIMIT 1))
		AND (($4::uuid IS NULL AND v.version=$5) OR v.uuid=$4)`,
		definition.OrgId, definition.DefinitionId, definition.ResourceType,
		nullableUUID(definition.ResourceTypeVersionUUID), version).
		Scan(&versionUUID, &number, asJson(&result.OutputsSchema), asJson(&result.ModuleContract))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrBadRequest("selected Resource Type version does not belong to this Module")
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve exact Resource Type contract")
	}
	if definition.ResourceTypeVersion != 0 && number != definition.ResourceTypeVersion {
		return nil, NewErrBadRequest("Resource Type version number and UUID disagree")
	}
	definition.ResourceTypeVersionUUID, definition.ResourceTypeVersion = versionUUID, number
	return &result, nil
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
