package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

type ModuleOperationReservation struct {
	ID                uuid.UUID
	OrgID             string
	ModuleUUID        uuid.UUID
	Namespace         string
	OperationID       uuid.UUID
	RelatedResourceID string
	Reason            string
	ResourceVersion   int64
	AcquiredBy        uuid.UUID
	AcquiredAt        time.Time
	ReleasedBy        *uuid.UUID
	ReleasedAt        *time.Time
	ReleaseReason     string
}

const moduleOperationReservationColumns = `id, org_id, module_uuid, namespace, operation_id, related_resource_id,
	reason, resource_version, acquired_by, acquired_at, released_by, released_at, COALESCE(release_reason, '')`

func scanModuleOperationReservation(row interface{ Scan(...any) error }) (*ModuleOperationReservation, error) {
	var result ModuleOperationReservation
	var releasedBy uuid.NullUUID
	var releasedAt sql.NullTime
	if err := row.Scan(&result.ID, &result.OrgID, &result.ModuleUUID, &result.Namespace, &result.OperationID,
		&result.RelatedResourceID, &result.Reason, &result.ResourceVersion, &result.AcquiredBy, &result.AcquiredAt,
		&releasedBy, &releasedAt, &result.ReleaseReason); err != nil {
		return nil, err
	}
	if releasedBy.Valid {
		value := releasedBy.UUID
		result.ReleasedBy = &value
	}
	if releasedAt.Valid {
		value := releasedAt.Time
		result.ReleasedAt = &value
	}
	return &result, nil
}

func (d *databaser) GetModuleOperationReservation(ctx context.Context, optionalTx Tx, orgID string, reservationID uuid.UUID, mode GetMode) (*ModuleOperationReservation, error) {
	result, err := scanModuleOperationReservation(d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT `+moduleOperationReservationColumns+`
		FROM module_operation_reservations WHERE org_id = $1 AND id = $2`+GetModeSuffix(mode), orgID, reservationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("Module operation reservation not found")
	}
	return result, errors.Wrap(err, "failed to get Module operation reservation")
}

func (d *databaser) ListModuleOperationReservations(ctx context.Context, optionalTx Tx, orgID string, moduleUUID uuid.UUID, includeReleased bool) ([]ModuleOperationReservation, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+moduleOperationReservationColumns+`
		FROM module_operation_reservations WHERE org_id = $1 AND module_uuid = $2
		AND ($3 OR released_at IS NULL) ORDER BY acquired_at DESC, id`, orgID, moduleUUID, includeReleased)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Module operation reservations")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleOperationReservation, 0)
	for rows.Next() {
		reservation, scanErr := scanModuleOperationReservation(rows)
		if scanErr != nil {
			return nil, errors.Wrap(scanErr, "failed to scan Module operation reservation")
		}
		result = append(result, *reservation)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate Module operation reservations")
}

func (d *databaser) AcquireModuleOperationReservation(ctx context.Context, tx Tx, orgID, moduleRef, namespace string, operationID uuid.UUID, relatedResourceID, reason string, actor uuid.UUID) (*ModuleOperationReservation, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if module.Status == moduleversions.CatalogueArchived {
		return nil, NewErrConflict("archived Module cannot accept a new operation reservation")
	}
	result, err := scanModuleOperationReservation(tx.QueryRowContext(ctx, `INSERT INTO module_operation_reservations
		(org_id, module_uuid, namespace, operation_id, related_resource_id, reason, acquired_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING
		RETURNING `+moduleOperationReservationColumns,
		orgID, module.UUID, namespace, operationID, relatedResourceID, strings.TrimSpace(reason), actor))
	if err == nil {
		return result, nil
	}
	// ON CONFLICT DO NOTHING makes an idempotency or exclusivity collision a
	// normal no-row result. In particular, it keeps the surrounding PostgreSQL
	// transaction usable while we inspect the existing reservation below.
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, errors.Wrap(err, "failed to acquire Module operation reservation")
	}
	existing, getErr := scanModuleOperationReservation(tx.QueryRowContext(ctx, `SELECT `+moduleOperationReservationColumns+`
		FROM module_operation_reservations WHERE org_id = $1 AND namespace = $2 AND operation_id = $3 FOR UPDATE`,
		orgID, namespace, operationID))
	if getErr == nil && existing.ModuleUUID == module.UUID && existing.RelatedResourceID == relatedResourceID && existing.ReleasedAt == nil {
		return existing, nil
	}
	blockers, listErr := d.ListModuleOperationReservations(ctx, tx, orgID, module.UUID, false)
	if listErr != nil {
		return nil, listErr
	}
	if len(blockers) > 0 {
		return nil, NewErrConflict("Module is reserved by " + blockers[0].Namespace + "/" + blockers[0].RelatedResourceID)
	}
	return nil, NewErrConflict("operation identity is already associated with a different Module reservation")
}

func (d *databaser) ReleaseModuleOperationReservation(ctx context.Context, tx Tx, orgID string, reservationID uuid.UUID, expectedVersion int64, actor uuid.UUID, reason string) (*ModuleOperationReservation, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	current, err := d.GetModuleOperationReservation(ctx, tx, orgID, reservationID, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != expectedVersion {
		return nil, NewErrConflict("Module operation reservation changed since it was read")
	}
	if current.ReleasedAt != nil {
		return current, nil
	}
	result, err := scanModuleOperationReservation(tx.QueryRowContext(ctx, `UPDATE module_operation_reservations
		SET released_by = $3, released_at = now(), release_reason = $4, resource_version = resource_version + 1
		WHERE org_id = $1 AND id = $2 RETURNING `+moduleOperationReservationColumns,
		orgID, reservationID, actor, strings.TrimSpace(reason)))
	return result, errors.Wrap(err, "failed to release Module operation reservation")
}

type ModuleExtensionContribution struct {
	ID                 uuid.UUID
	OrgID              string
	ModuleUUID         uuid.UUID
	VersionUUID        *uuid.UUID
	EnvironmentUUID    *uuid.UUID
	Namespace          string
	ExternalResourceID string
	Kind               string
	LifecycleState     string
	Label              string
	TargetURL          string
	Payload            map[string]any
	CreatedBy          uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const moduleExtensionContributionColumns = `id, org_id, module_uuid, version_uuid, environment_uuid, namespace, external_resource_id,
	contribution_kind, lifecycle_state, label, COALESCE(target_url, ''), payload, created_by, created_at, updated_at`

func scanModuleExtensionContribution(row interface{ Scan(...any) error }) (*ModuleExtensionContribution, error) {
	var result ModuleExtensionContribution
	var versionUUID uuid.NullUUID
	var environmentUUID uuid.NullUUID
	var payload []byte
	if err := row.Scan(&result.ID, &result.OrgID, &result.ModuleUUID, &versionUUID, &environmentUUID, &result.Namespace,
		&result.ExternalResourceID, &result.Kind, &result.LifecycleState, &result.Label, &result.TargetURL,
		&payload, &result.CreatedBy, &result.CreatedAt, &result.UpdatedAt); err != nil {
		return nil, err
	}
	if versionUUID.Valid {
		value := versionUUID.UUID
		result.VersionUUID = &value
	}
	if environmentUUID.Valid {
		value := environmentUUID.UUID
		result.EnvironmentUUID = &value
	}
	if err := json.Unmarshal(payload, &result.Payload); err != nil {
		return nil, errors.Wrap(err, "failed to decode Module extension payload")
	}
	return &result, nil
}

func (d *databaser) UpsertModuleExtensionContribution(ctx context.Context, tx Tx, orgID, moduleRef string, versionUUID, environmentUUID *uuid.UUID, namespace, externalResourceID, kind, lifecycleState, label, targetURL string, payload map[string]any, actor uuid.UUID) (*ModuleExtensionContribution, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeDefault)
	if err != nil {
		return nil, err
	}
	if versionUUID != nil {
		if _, err := d.GetCoreModuleVersion(ctx, tx, orgID, module.UUID.String(), versionUUID.String(), GetModeDefault); err != nil {
			return nil, err
		}
	}
	if environmentUUID != nil {
		if _, err := d.GetEnvironmentByUuid(ctx, tx, orgID, *environmentUUID, GetModeDefault); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, NewErrBadRequest("extension payload must be valid JSON")
	}
	result, err := scanModuleExtensionContribution(tx.QueryRowContext(ctx, `INSERT INTO module_extension_contributions
		(org_id, module_uuid, version_uuid, environment_uuid, namespace, external_resource_id, contribution_kind, lifecycle_state, label, target_url, payload, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12)
		ON CONFLICT (org_id, namespace, external_resource_id, contribution_kind) DO UPDATE SET
			version_uuid = EXCLUDED.version_uuid, environment_uuid = EXCLUDED.environment_uuid,
			lifecycle_state = EXCLUDED.lifecycle_state, label = EXCLUDED.label,
			target_url = EXCLUDED.target_url, payload = EXCLUDED.payload, updated_at = now()
		WHERE module_extension_contributions.module_uuid = EXCLUDED.module_uuid
		RETURNING `+moduleExtensionContributionColumns, orgID, module.UUID, versionUUID, environmentUUID, namespace,
		externalResourceID, kind, lifecycleState, label, targetURL, encoded, actor))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrConflict("extension contribution identity is already bound to another Module")
	}
	return result, errors.Wrap(err, "failed to register Module extension contribution")
}

func (d *databaser) ListModuleExtensionContributionsForEnvironment(ctx context.Context, optionalTx Tx, orgID string, environmentUUID uuid.UUID, includeDraft, includeTerminal bool) ([]ModuleExtensionContribution, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+moduleExtensionContributionColumns+`
		FROM module_extension_contributions WHERE org_id = $1 AND environment_uuid = $2
		AND ($3 OR lifecycle_state <> 'draft') AND ($4 OR lifecycle_state <> 'terminal')
		ORDER BY created_at DESC, id`, orgID, environmentUUID, includeDraft, includeTerminal)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Environment Module extension contributions")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleExtensionContribution, 0)
	for rows.Next() {
		contribution, scanErr := scanModuleExtensionContribution(rows)
		if scanErr != nil {
			return nil, errors.Wrap(scanErr, "failed to scan Environment Module extension contribution")
		}
		result = append(result, *contribution)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate Environment Module extension contributions")
}

func (d *databaser) ListModuleExtensionContributions(ctx context.Context, optionalTx Tx, orgID string, moduleUUID uuid.UUID, includeDraft, includeTerminal bool) ([]ModuleExtensionContribution, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+moduleExtensionContributionColumns+`
		FROM module_extension_contributions WHERE org_id = $1 AND module_uuid = $2
		AND ($3 OR lifecycle_state <> 'draft') AND ($4 OR lifecycle_state <> 'terminal')
		ORDER BY created_at DESC, id`, orgID, moduleUUID, includeDraft, includeTerminal)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Module extension contributions")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleExtensionContribution, 0)
	for rows.Next() {
		contribution, scanErr := scanModuleExtensionContribution(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, *contribution)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate Module extension contributions")
}
