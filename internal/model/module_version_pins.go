package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

const (
	eventFieldActivationEventID = "activation_event_id"
	eventFieldActorType         = "actor_type"
	eventFieldBulkOperationID   = "bulk_operation_id"
	eventFieldEnvironmentUUID   = "environment_uuid"
	eventFieldPinID             = "pin_id"
	eventFieldProjectUUID       = "project_uuid"
	eventFieldRevision          = "revision"
	eventFieldTransitionID      = "transition_id"
)

type EnvironmentModuleVersionPin struct {
	ID                        uuid.UUID
	OrgID                     string
	ProjectUUID               uuid.UUID
	ProjectID                 string
	EnvironmentUUID           uuid.UUID
	EnvironmentID             string
	ModuleUUID                uuid.UUID
	VersionUUID               uuid.UUID
	Status                    moduleversions.PinStatus
	ResourceVersion           int64
	ActivationEventID         uuid.UUID
	BulkOperationID           *uuid.UUID
	OverrideOperationID       *uuid.UUID
	OverrideTargetVersionUUID *uuid.UUID
	OverrideActor             *uuid.UUID
	OverrideReason            string
	OverrideDeploymentID      *uuid.UUID
	CreatedBy                 uuid.UUID
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	RemovedAt                 *time.Time
}

type ModuleVersionPinEvent struct {
	Sequence          int64
	ID                uuid.UUID
	PinID             uuid.UUID
	Revision          int64
	EventType         string
	FromStatus        *moduleversions.PinStatus
	ToStatus          moduleversions.PinStatus
	ActivationEventID uuid.UUID
	Actor             uuid.UUID
	ActorType         string
	Reason            string
	OperationID       *uuid.UUID
	DeploymentID      *uuid.UUID
	BulkOperationID   *uuid.UUID
	CreatedAt         time.Time
}

func scanEnvironmentModuleVersionPin(row interface{ Scan(...any) error }) (*EnvironmentModuleVersionPin, error) {
	var result EnvironmentModuleVersionPin
	var bulkOperationID, overrideOperationID, overrideTargetVersionUUID, overrideActor, overrideDeploymentID uuid.NullUUID
	var removedAt sql.NullTime
	err := row.Scan(&result.ID, &result.OrgID, &result.ProjectUUID, &result.ProjectID, &result.EnvironmentUUID,
		&result.EnvironmentID, &result.ModuleUUID,
		&result.VersionUUID, &result.Status, &result.ResourceVersion, &result.ActivationEventID, &bulkOperationID,
		&overrideOperationID, &overrideTargetVersionUUID, &overrideActor, &result.OverrideReason, &overrideDeploymentID,
		&result.CreatedBy, &result.CreatedAt, &result.UpdatedAt, &removedAt)
	if err != nil {
		return nil, err
	}
	for source, target := range map[*uuid.NullUUID]**uuid.UUID{
		&bulkOperationID: &result.BulkOperationID, &overrideOperationID: &result.OverrideOperationID,
		&overrideTargetVersionUUID: &result.OverrideTargetVersionUUID, &overrideActor: &result.OverrideActor,
		&overrideDeploymentID: &result.OverrideDeploymentID,
	} {
		if source.Valid {
			value := source.UUID
			*target = &value
		}
	}
	if removedAt.Valid {
		result.RemovedAt = &removedAt.Time
	}
	return &result, nil
}

const environmentModuleVersionPinColumns = `id, org_id, project_uuid, project_id, environment_uuid, environment_id, module_uuid, version_uuid,
	status, resource_version, activation_event_id, bulk_operation_id, override_operation_id,
	override_target_version_uuid, override_actor, COALESCE(override_reason, ''), override_deployment_id,
	created_by, created_at, updated_at, removed_at`

func (d *databaser) GetEnvironmentModuleVersionPin(ctx context.Context, optionalTx Tx, orgID string, pinID uuid.UUID, mode GetMode) (*EnvironmentModuleVersionPin, error) {
	pin, err := scanEnvironmentModuleVersionPin(d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT `+environmentModuleVersionPinColumns+`
		FROM environment_module_version_pins WHERE org_id = $1 AND id = $2`+GetModeSuffix(mode), orgID, pinID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("module version pin not found")
	}
	return pin, errors.Wrap(err, "failed to get module version pin")
}

func (d *databaser) ListEnvironmentModuleVersionPins(ctx context.Context, optionalTx Tx, orgID string, environmentUUID *uuid.UUID, moduleUUID *uuid.UUID, includeRemoved bool) ([]EnvironmentModuleVersionPin, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+environmentModuleVersionPinColumns+`
		FROM environment_module_version_pins WHERE org_id = $1
		AND ($2::uuid IS NULL OR environment_uuid = $2) AND ($3::uuid IS NULL OR module_uuid = $3)
		AND ($4 OR status <> 'removed') ORDER BY created_at DESC, id`, orgID, environmentUUID, moduleUUID, includeRemoved)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list module version pins")
	}
	defer func() { _ = rows.Close() }()
	result := make([]EnvironmentModuleVersionPin, 0)
	for rows.Next() {
		pin, err := scanEnvironmentModuleVersionPin(rows)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan module version pin")
		}
		result = append(result, *pin)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate module version pins")
}

func (d *databaser) CreateEnvironmentModuleVersionPin(ctx context.Context, tx Tx, orgID string, projectUUID uuid.UUID, projectID string, environmentUUID uuid.UUID, environmentID string, moduleUUID, versionUUID, actor uuid.UUID, actorType, reason string, bulkOperationID *uuid.UUID, allowDefective bool) (*EnvironmentModuleVersionPin, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	var lifecycle moduleversions.LifecycleStatus
	if err := tx.QueryRowContext(ctx, `SELECT semantic_status FROM definition_versions
		WHERE org_id = $1 AND module_uuid = $2 AND uuid = $3 FOR SHARE`, orgID, moduleUUID, versionUUID).Scan(&lifecycle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NewErrNotFound("module version not found")
		}
		return nil, errors.Wrap(err, "failed to validate pinned module version")
	}
	if lifecycle == moduleversions.LifecycleDefective && !allowDefective {
		return nil, NewErrBadRequest("pinning a Defective version requires the dedicated capability and exact confirmation")
	}
	activationEventID := uuid.New()
	pin, err := scanEnvironmentModuleVersionPin(tx.QueryRowContext(ctx, `INSERT INTO environment_module_version_pins
		(org_id, project_uuid, project_id, environment_uuid, environment_id, module_uuid, version_uuid, activation_event_id, bulk_operation_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+environmentModuleVersionPinColumns,
		orgID, projectUUID, projectID, environmentUUID, environmentID, moduleUUID, versionUUID, activationEventID, bulkOperationID, actor))
	if err != nil {
		if isUniqueViolation(err, "environment_module_version_pins_protected") {
			return nil, NewErrConflict("environment already has an active or override-pending Pin for this module")
		}
		return nil, errors.Wrap(err, "failed to create module version pin")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO environment_module_version_pin_events
		(id, pin_id, revision, event_type, to_status, activation_event_id, actor, actor_type, reason, bulk_operation_id)
		VALUES ($1, $2, 1, 'created', 'active', $1, $3, $4, $5, $6)`, activationEventID, pin.ID, actor, actorType, reason, bulkOperationID); err != nil {
		return nil, errors.Wrap(err, "failed to append pin creation event")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.version.pin-lifecycle-changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldPinID: pin.ID, eventFieldEnvironmentUUID: environmentUUID, eventFieldProjectUUID: projectUUID,
		eventFieldModuleUUID: moduleUUID, eventFieldVersionUUID: versionUUID, eventFieldTransitionID: activationEventID,
		eventFieldRevision: pin.ResourceVersion, eventFieldToStatus: pin.Status, eventFieldActivationEventID: pin.ActivationEventID,
		eventFieldActor: actor, eventFieldActorType: actorType, eventFieldReason: reason, eventFieldBulkOperationID: bulkOperationID,
	}); err != nil {
		return nil, err
	}
	return pin, nil
}

func (d *databaser) TransitionEnvironmentModuleVersionPin(ctx context.Context, tx Tx, orgID string, pinID uuid.UUID, expectedVersion int64, target moduleversions.PinStatus, eventType string, actor uuid.UUID, actorType, reason string, operationID, targetVersionUUID, deploymentID *uuid.UUID) (*EnvironmentModuleVersionPin, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	pin, err := d.GetEnvironmentModuleVersionPin(ctx, tx, orgID, pinID, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if pin.ResourceVersion != expectedVersion {
		return nil, NewErrConflict("module version pin changed since it was read")
	}
	if pin.Status == moduleversions.PinOverridePending && target == moduleversions.PinRemoved {
		return nil, NewErrConflict("override-pending Pin is locked by its owning add-on operation")
	}
	if err := moduleversions.ValidatePinTransition(pin.Status, target); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if pin.Status == target {
		return pin, nil
	}
	if target == moduleversions.PinOverridePending && (operationID == nil || targetVersionUUID == nil) {
		return nil, NewErrBadRequest("override_pending requires an exact operation and target version")
	}
	if (pin.Status == moduleversions.PinOverridePending || pin.Status == moduleversions.PinOverridden) && operationID != nil && (pin.OverrideOperationID == nil || *pin.OverrideOperationID != *operationID) {
		return nil, NewErrConflict("Pin is owned by a different add-on operation")
	}
	eventID := uuid.New()
	activationEventID := pin.ActivationEventID
	if target == moduleversions.PinActive && pin.Status == moduleversions.PinOverridden {
		activationEventID = eventID
	}
	removedAt := any(nil)
	if target == moduleversions.PinRemoved {
		removedAt = time.Now().UTC()
	}
	updated, err := scanEnvironmentModuleVersionPin(tx.QueryRowContext(ctx, `UPDATE environment_module_version_pins SET
		status = $3, resource_version = resource_version + 1, activation_event_id = $4,
		override_operation_id = CASE WHEN $3 = 'active' OR $3 = 'removed' THEN NULL ELSE COALESCE($5, override_operation_id) END,
		override_target_version_uuid = CASE WHEN $3 = 'active' OR $3 = 'removed' THEN NULL ELSE COALESCE($6, override_target_version_uuid) END,
		override_actor = CASE WHEN $3 = 'active' OR $3 = 'removed' THEN NULL::uuid ELSE $7::uuid END,
		override_reason = CASE WHEN $3 = 'active' OR $3 = 'removed' THEN NULL ELSE $8 END,
		override_deployment_id = CASE WHEN $3 = 'active' OR $3 = 'removed' THEN NULL ELSE COALESCE($9, override_deployment_id) END,
		removed_at = $10, updated_at = now()
		WHERE org_id = $1 AND id = $2 RETURNING `+environmentModuleVersionPinColumns,
		orgID, pinID, target, activationEventID, operationID, targetVersionUUID, actor, reason, deploymentID, removedAt))
	if err != nil {
		return nil, errors.Wrap(err, "failed to transition module version pin")
	}
	var eventRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1
		FROM environment_module_version_pin_events WHERE pin_id = $1`, pinID).Scan(&eventRevision); err != nil {
		return nil, errors.Wrap(err, "failed to allocate Pin event revision")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO environment_module_version_pin_events
		(id, pin_id, revision, event_type, from_status, to_status, activation_event_id, actor, actor_type, reason, operation_id, deployment_id, bulk_operation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, eventID, pinID,
		eventRevision, eventType, pin.Status, target, updated.ActivationEventID, actor, actorType, reason,
		operationID, deploymentID, pin.BulkOperationID); err != nil {
		return nil, errors.Wrap(err, "failed to append Pin lifecycle event")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.version.pin-lifecycle-changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldPinID: updated.ID, eventFieldEnvironmentUUID: updated.EnvironmentUUID,
		eventFieldProjectUUID: updated.ProjectUUID, eventFieldModuleUUID: updated.ModuleUUID, eventFieldVersionUUID: updated.VersionUUID,
		eventFieldTransitionID: eventID, eventFieldRevision: eventRevision, eventFieldFromStatus: pin.Status, eventFieldToStatus: updated.Status,
		eventFieldActivationEventID: updated.ActivationEventID, eventFieldActor: actor, eventFieldActorType: actorType, eventFieldReason: reason,
		"operation_id": operationID, "deployment_id": deploymentID, eventFieldBulkOperationID: updated.BulkOperationID,
	}); err != nil {
		return nil, err
	}
	return updated, nil
}

// AppendModuleVersionPinNote records context without mutating the Pin or moving
// its immutable activation boundary. Locking the Pin serialises note revisions
// with lifecycle transitions while leaving resource_version untouched.
func (d *databaser) AppendModuleVersionPinNote(ctx context.Context, tx Tx, orgID string, pinID uuid.UUID, actor uuid.UUID, actorType, note string) (*ModuleVersionPinEvent, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(note); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	pin, err := d.GetEnvironmentModuleVersionPin(ctx, tx, orgID, pinID, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if pin.Status == moduleversions.PinRemoved {
		return nil, NewErrConflict("notes cannot be appended to a removed Pin")
	}

	event := ModuleVersionPinEvent{
		ID:                uuid.New(),
		PinID:             pin.ID,
		EventType:         "note_added",
		ToStatus:          pin.Status,
		ActivationEventID: pin.ActivationEventID,
		Actor:             actor,
		ActorType:         actorType,
		Reason:            note,
		BulkOperationID:   pin.BulkOperationID,
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1
		FROM environment_module_version_pin_events WHERE pin_id = $1`, pin.ID).Scan(&event.Revision); err != nil {
		return nil, errors.Wrap(err, "failed to allocate Pin note revision")
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO environment_module_version_pin_events
		(id, pin_id, revision, event_type, from_status, to_status, activation_event_id, actor, actor_type, reason, bulk_operation_id)
		VALUES ($1, $2, $3, 'note_added', $4, $4, $5, $6, $7, $8, $9)
		RETURNING sequence, created_at`, event.ID, pin.ID, event.Revision, pin.Status, pin.ActivationEventID,
		actor, actorType, note, pin.BulkOperationID).Scan(&event.Sequence, &event.CreatedAt); err != nil {
		return nil, errors.Wrap(err, "failed to append Pin note")
	}
	fromStatus := pin.Status
	event.FromStatus = &fromStatus
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.version.pin-lifecycle-changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldPinID: pin.ID, eventFieldEnvironmentUUID: pin.EnvironmentUUID,
		eventFieldProjectUUID: pin.ProjectUUID, eventFieldModuleUUID: pin.ModuleUUID, eventFieldVersionUUID: pin.VersionUUID,
		eventFieldTransitionID: event.ID, eventFieldRevision: event.Revision, "event_type": event.EventType,
		eventFieldFromStatus: pin.Status, eventFieldToStatus: pin.Status, eventFieldActivationEventID: pin.ActivationEventID,
		eventFieldActor: actor, eventFieldActorType: actorType, "note": note, eventFieldBulkOperationID: pin.BulkOperationID,
	}); err != nil {
		return nil, err
	}
	return &event, nil
}

func (d *databaser) ListModuleVersionPinEvents(ctx context.Context, optionalTx Tx, orgID string, pinID uuid.UUID) ([]ModuleVersionPinEvent, error) {
	if _, err := d.GetEnvironmentModuleVersionPin(ctx, optionalTx, orgID, pinID, GetModeDefault); err != nil {
		return nil, err
	}
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT sequence, id, pin_id, revision, event_type,
		from_status, to_status, activation_event_id, actor, actor_type, reason, operation_id, deployment_id,
		bulk_operation_id, created_at FROM environment_module_version_pin_events WHERE pin_id = $1 ORDER BY sequence`, pinID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Pin events")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleVersionPinEvent, 0)
	for rows.Next() {
		var item ModuleVersionPinEvent
		var fromStatus sql.NullString
		var operationID, deploymentID, bulkOperationID uuid.NullUUID
		if err := rows.Scan(&item.Sequence, &item.ID, &item.PinID, &item.Revision, &item.EventType, &fromStatus,
			&item.ToStatus, &item.ActivationEventID, &item.Actor, &item.ActorType, &item.Reason, &operationID,
			&deploymentID, &bulkOperationID, &item.CreatedAt); err != nil {
			return nil, errors.Wrap(err, "failed to scan Pin event")
		}
		if fromStatus.Valid {
			value := moduleversions.PinStatus(fromStatus.String)
			item.FromStatus = &value
		}
		for source, target := range map[*uuid.NullUUID]**uuid.UUID{
			&operationID: &item.OperationID, &deploymentID: &item.DeploymentID, &bulkOperationID: &item.BulkOperationID,
		} {
			if source.Valid {
				value := source.UUID
				*target = &value
			}
		}
		result = append(result, item)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate Pin events")
}

func (d *databaser) RemoveEnvironmentModuleVersionPinsForDeletion(ctx context.Context, tx Tx, orgID string, environmentUUID, actor uuid.UUID, actorType string) ([]EnvironmentModuleVersionPin, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+environmentModuleVersionPinColumns+`
		FROM environment_module_version_pins WHERE org_id = $1 AND environment_uuid = $2
		AND status <> 'removed' ORDER BY id FOR UPDATE`, orgID, environmentUUID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to lock Environment Pins for deletion")
	}
	defer func() { _ = rows.Close() }()
	pins := make([]EnvironmentModuleVersionPin, 0)
	for rows.Next() {
		pin, err := scanEnvironmentModuleVersionPin(rows)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan Environment Pin for deletion")
		}
		pins = append(pins, *pin)
	}
	if err := rows.Close(); err != nil {
		return nil, errors.Wrap(err, "failed to close Environment Pin deletion rows")
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "failed to iterate Environment Pins for deletion")
	}
	for _, pin := range pins {
		if pin.Status == moduleversions.PinOverridePending {
			return nil, NewErrConflict(fmt.Sprintf("Environment deletion is blocked by override-pending Pin %s owned by operation %s", pin.ID, pin.OverrideOperationID))
		}
	}
	removed := make([]EnvironmentModuleVersionPin, 0, len(pins))
	for _, pin := range pins {
		updated, err := d.TransitionEnvironmentModuleVersionPin(ctx, tx, orgID, pin.ID, pin.ResourceVersion,
			moduleversions.PinRemoved, "environment_deleted", actor, actorType, "environment_deleted", nil, nil, nil)
		if err != nil {
			return nil, err
		}
		removed = append(removed, *updated)
	}
	return removed, nil
}
