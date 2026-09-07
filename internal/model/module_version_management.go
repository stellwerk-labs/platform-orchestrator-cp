package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
)

const (
	catalogueStatusActive   = "active"
	catalogueStatusArchived = "archived"
	eventFieldActor         = "actor"
	eventFieldFromStatus    = "from_status"
	eventFieldModuleSlug    = "module_slug"
	eventFieldModuleUUID    = "module_uuid"
	eventFieldOrgID         = "org_id"
	eventFieldReason        = "reason"
	eventFieldResourceVer   = "resource_version"
	eventFieldSemanticVer   = "semantic_version"
	eventFieldToStatus      = "to_status"
	eventFieldVersionUUID   = "version_uuid"
)

func (d *databaser) appendModuleCoreOutboxEvent(ctx context.Context, tx Tx, eventType string, data map[string]any) error {
	if _, err := d.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.EventType(eventType), Time: time.Now().UTC(), Data: data,
	})); err != nil {
		return errors.Wrap(err, "failed to append Module Core event to standard outbox")
	}
	return nil
}

type ModuleCatalogue struct {
	OrgID                      string
	UUID                       uuid.UUID
	Slug                       string
	DisplayName                string
	Description                string
	ResourceType               string
	Tags                       map[string]string
	Status                     moduleversions.CatalogueStatus
	ResourceVersion            int64
	CurrentDefaultVersionUUID  *uuid.UUID
	PreviousDefaultVersionUUID *uuid.UUID
	ManagedDefaultGeneration   int64
	CreatedAt                  time.Time
	ArchivedAt                 *time.Time
	ArchivedBy                 *uuid.UUID
	ArchiveReason              string
}

type CoreModuleVersion struct {
	OrgID               string
	ModuleUUID          uuid.UUID
	ModuleSlug          string
	UUID                uuid.UUID
	SemanticVersion     *string
	OpaqueVersionID     string
	MigrationGeneration string
	ArtifactDigest      string
	VerificationStatus  moduleversions.VerificationStatus
	LifecycleStatus     moduleversions.LifecycleStatus
	SourceRevision      string
	ReleaseNotes        *string
	ResourceVersion     int64
	PublishedBy         *uuid.UUID
	CreatedAt           time.Time
	Definition          ModuleDefinitionVersion
}

type ModuleLifecycleEvent struct {
	Sequence        int64
	ID              uuid.UUID
	ModuleUUID      uuid.UUID
	VersionUUID     uuid.UUID
	FromStatus      *moduleversions.LifecycleStatus
	ToStatus        moduleversions.LifecycleStatus
	ResourceVersion int64
	Actor           uuid.UUID
	Reason          *string
	CorrelationID   *uuid.UUID
	Payload         json.RawMessage
	CreatedAt       time.Time
}

type StableModuleVersionSuccessor struct {
	Prerelease    CoreModuleVersion
	Stable        CoreModuleVersion
	CorrelationID uuid.UUID
}

func (d *databaser) managedDefaultLineageHead(ctx context.Context, tx Tx, module ModuleCatalogue) (*CoreModuleVersion, error) {
	if module.CurrentDefaultVersionUUID != nil {
		return d.GetCoreModuleVersion(ctx, tx, module.OrgID, module.UUID.String(), module.CurrentDefaultVersionUUID.String(), GetModeDefault)
	}
	var defectiveDefaultUUID uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT version_uuid FROM module_version_lifecycle_events
		WHERE module_uuid = $1 AND from_status = 'default' AND to_status = 'defective'
		ORDER BY sequence DESC LIMIT 1`, module.UUID).Scan(&defectiveDefaultUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve Default lineage head")
	}
	return d.GetCoreModuleVersion(ctx, tx, module.OrgID, module.UUID.String(), defectiveDefaultUUID.String(), GetModeDefault)
}

func (d *databaser) PublishCoreModuleVersion(ctx context.Context, tx Tx, request *ModuleDefinitionVersion, actor uuid.UUID) (*CoreModuleVersion, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	module, err := d.GetModuleCatalogue(ctx, tx, request.OrgId, request.DefinitionId, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if module.Status != moduleversions.CatalogueActive {
		return nil, NewErrConflict("archived modules do not accept version publication")
	}
	if request.ResourceType != "" && request.ResourceType != module.ResourceType {
		return nil, NewErrBadRequest("a module version cannot change the module's immutable Resource Type binding")
	}
	candidate, err := moduleversions.ParseVersion(request.SemanticVersion)
	if err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	lineageHead, err := d.managedDefaultLineageHead(ctx, tx, *module)
	if err != nil {
		return nil, err
	}
	if lineageHead != nil {
		current := lineageHead
		if current.SemanticVersion != nil {
			defaultVersion, err := moduleversions.ParseVersion(*current.SemanticVersion)
			if err != nil {
				return nil, errors.Wrap(err, "stored default module version is invalid")
			}
			if err := moduleversions.ValidatePublicationVersion(candidate, &defaultVersion); err != nil {
				return nil, NewErrBadRequest(err.Error())
			}
		}
	}
	request.VersionId = request.SemanticVersion
	request.SemanticStatus = string(moduleversions.LifecycleProposed)
	request.MigrationGeneration = "managed"
	request.VerificationStatus = string(moduleversions.VerificationUnverified)
	request.PublishedBy = &actor
	request.ResourceType = module.ResourceType
	created, err := d.CreateModuleDefinitionVersion(ctx, tx, request)
	if err != nil {
		if isUniqueViolation(err, "definition_versions_one_proposed") {
			return nil, NewErrConflict("module already has a Proposed version")
		}
		if isUniqueViolation(err, "definition_versions_semver_unique", "definition_versions_pkey") {
			return nil, NewErrConflict("Module Version SemVer is permanently reserved")
		}
		return nil, errors.Wrap(err, "failed to publish module version")
	}
	return d.GetCoreModuleVersion(ctx, tx, request.OrgId, request.DefinitionId, created.VersionUUID.String(), GetModeDefault)
}

func (d *databaser) PublishStableModuleVersionSuccessor(ctx context.Context, tx Tx, orgID, moduleRef, prereleaseRef string, expectedPrereleaseVersion int64, reason string, request *ModuleDefinitionVersion, actor uuid.UUID) (*StableModuleVersionSuccessor, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if _, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate); err != nil {
		return nil, err
	}
	prerelease, err := d.GetCoreModuleVersion(ctx, tx, orgID, moduleRef, prereleaseRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if prerelease.ResourceVersion != expectedPrereleaseVersion {
		return nil, NewErrConflict("prerelease Module Version changed since it was read")
	}
	if prerelease.LifecycleStatus != moduleversions.LifecycleProposed || prerelease.SemanticVersion == nil {
		return nil, NewErrConflict("stable graduation requires the exact Proposed prerelease")
	}
	parsedPrerelease, err := moduleversions.ParseVersion(*prerelease.SemanticVersion)
	if err != nil {
		return nil, errors.Wrap(err, "stored prerelease Module Version is invalid")
	}
	if len(parsedPrerelease.PreRelease) == 0 {
		return nil, NewErrBadRequest("stable graduation requires a prerelease Module Version")
	}
	stable, err := moduleversions.ParseVersion(request.SemanticVersion)
	if err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if len(stable.PreRelease) != 0 {
		return nil, NewErrBadRequest("stable successor must not contain a prerelease component")
	}
	if stable.Compare(parsedPrerelease) <= 0 {
		return nil, NewErrBadRequest("stable successor must have higher SemVer precedence than the expected prerelease")
	}

	correlationID := uuid.New()
	updatedPrerelease, err := d.TransitionCoreModuleVersion(ctx, tx, orgID, moduleRef, prerelease.UUID.String(),
		moduleversions.LifecycleDeprecated, expectedPrereleaseVersion, actor, reason, &correlationID)
	if err != nil {
		return nil, err
	}
	request.OrgId = orgID
	request.DefinitionId = prerelease.ModuleSlug
	request.ModuleUUID = prerelease.ModuleUUID
	published, err := d.PublishCoreModuleVersion(ctx, tx, request, actor)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE module_version_lifecycle_events SET correlation_id = $2
		WHERE version_uuid = $1 AND from_status IS NULL`, published.UUID, correlationID); err != nil {
		return nil, errors.Wrap(err, "failed to correlate stable successor publication event")
	}
	return &StableModuleVersionSuccessor{Prerelease: *updatedPrerelease, Stable: *published, CorrelationID: correlationID}, nil
}

func scanModuleCatalogue(row interface{ Scan(...any) error }) (*ModuleCatalogue, error) {
	var result ModuleCatalogue
	var currentDefault, previousDefault, archivedBy uuid.NullUUID
	var archivedAt sql.NullTime
	err := row.Scan(&result.OrgID, &result.UUID, &result.Slug, &result.DisplayName, &result.Description,
		&result.ResourceType, asJson(&result.Tags), &result.Status, &result.ResourceVersion,
		&currentDefault, &previousDefault, &result.ManagedDefaultGeneration, &result.CreatedAt,
		&archivedAt, &archivedBy, &result.ArchiveReason)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.DisplayName) == "" {
		result.DisplayName = result.Slug
	}
	if currentDefault.Valid {
		value := currentDefault.UUID
		result.CurrentDefaultVersionUUID = &value
	}
	if previousDefault.Valid {
		value := previousDefault.UUID
		result.PreviousDefaultVersionUUID = &value
	}
	if archivedAt.Valid {
		result.ArchivedAt = &archivedAt.Time
	}
	if archivedBy.Valid {
		value := archivedBy.UUID
		result.ArchivedBy = &value
	}
	return &result, nil
}

func (d *databaser) ListModuleCatalogues(ctx context.Context, optionalTx Tx, orgID string, includeArchived bool) ([]ModuleCatalogue, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+moduleCatalogueColumns+`
		FROM definitions WHERE org_id = $1 AND ($2 OR catalogue_status = 'active')
		ORDER BY lower(display_name), id`, orgID, includeArchived)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Module catalogue")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleCatalogue, 0)
	for rows.Next() {
		entry, err := scanModuleCatalogue(rows)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan Module catalogue entry")
		}
		result = append(result, *entry)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate Module catalogue")
}

const moduleCatalogueColumns = `org_id, uuid, id, display_name, COALESCE(description, ''), resource_type, tags,
	catalogue_status, resource_version, current_default_version_uuid, previous_default_version_uuid,
	managed_default_generation, created_at, archived_at, archived_by, COALESCE(archive_reason, '')`

func (d *databaser) CreateEmptyModule(ctx context.Context, tx Tx, orgID, slug, displayName, description, resourceType string, tags map[string]string) (*ModuleCatalogue, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = slug
	}
	module, err := scanModuleCatalogue(tx.QueryRowContext(ctx, `INSERT INTO definitions
		(org_id, id, created_at, resource_type, latest_version_id, display_name, description, tags)
		VALUES ($1, $2, now(), $3, NULL, $4, $5, $6) RETURNING `+moduleCatalogueColumns,
		orgID, slug, resourceType, strings.TrimSpace(displayName), description, asJson(&tags)))
	if isUniqueViolation(err, "definitions_pkey") {
		return nil, NewErrConflict("Module technical slug is already reserved")
	}
	if pqError := new(pq.Error); errors.As(err, &pqError) && pqError.Constraint == "definitions_org_id_fkey" {
		return nil, NewErrNotFound("organization not found")
	}
	return module, errors.Wrap(err, "failed to create empty Module shell")
}

func (d *databaser) GetModuleCatalogue(ctx context.Context, optionalTx Tx, orgID, moduleRef string, mode GetMode) (*ModuleCatalogue, error) {
	module, err := scanModuleCatalogue(d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT `+moduleCatalogueColumns+`
		FROM definitions WHERE org_id = $1 AND (id = $2 OR uuid::text = $2)`+GetModeSuffix(mode), orgID, moduleRef))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("module not found")
	}
	return module, errors.Wrap(err, "failed to get module catalogue entry")
}

func (d *databaser) UpdateModuleCatalogueMetadata(ctx context.Context, tx Tx, orgID, moduleRef, displayName, description string, tags map[string]string, expectedVersion int64) (*ModuleCatalogue, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	module, err := scanModuleCatalogue(tx.QueryRowContext(ctx, `UPDATE definitions SET
			display_name = $3, description = $4, tags = $5, resource_version = resource_version + 1
		WHERE org_id = $1 AND (id = $2 OR uuid::text = $2) AND resource_version = $6
		RETURNING `+moduleCatalogueColumns, orgID, moduleRef, displayName, description, asJson(&tags), expectedVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrConflict("module changed since it was read")
	}
	return module, errors.Wrap(err, "failed to update module catalogue metadata")
}

func (d *databaser) SetModuleCatalogueStatus(ctx context.Context, tx Tx, orgID, moduleRef string, target moduleversions.CatalogueStatus, actor uuid.UUID, reason string, expectedVersion int64) (*ModuleCatalogue, error) {
	if tx == nil {
		return nil, errors.New("transaction required")
	}
	if err := moduleversions.RequireTransitionReason(reason); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	current, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != expectedVersion {
		return nil, NewErrConflict("module changed since it was read")
	}
	if current.Status == target {
		return current, nil
	}
	if target != moduleversions.CatalogueActive && target != moduleversions.CatalogueArchived {
		return nil, NewErrBadRequest("invalid module catalogue status")
	}
	var row *sql.Row
	if target == moduleversions.CatalogueArchived {
		reservations, listErr := d.ListModuleOperationReservations(ctx, tx, orgID, current.UUID, false)
		if listErr != nil {
			return nil, listErr
		}
		if len(reservations) > 0 {
			blockers := make([]string, 0, len(reservations))
			for _, reservation := range reservations {
				blockers = append(blockers, fmt.Sprintf("%s/%s (operation %s)", reservation.Namespace, reservation.RelatedResourceID, reservation.OperationID))
			}
			return nil, NewErrConflict("Module has exclusive operation reservations: " + strings.Join(blockers, ", "))
		}
		row = tx.QueryRowContext(ctx, `UPDATE definitions SET catalogue_status = 'archived', archived_at = now(),
			archived_by = $3, archive_reason = $4, resource_version = resource_version + 1
			WHERE org_id = $1 AND uuid = $2 RETURNING `+moduleCatalogueColumns, orgID, current.UUID, actor, strings.TrimSpace(reason))
	} else {
		row = tx.QueryRowContext(ctx, `UPDATE definitions SET catalogue_status = 'active', archived_at = NULL,
			archived_by = NULL, archive_reason = NULL, resource_version = resource_version + 1
			WHERE org_id = $1 AND uuid = $2 RETURNING `+moduleCatalogueColumns, orgID, current.UUID)
	}
	updated, err := scanModuleCatalogue(row)
	if err != nil {
		return nil, errors.Wrap(err, "failed to change module catalogue status")
	}
	eventType := "module.archived"
	if target == moduleversions.CatalogueActive {
		eventType = "module.unarchived"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO module_catalogue_events
		(org_id, module_uuid, event_type, module_resource_version, actor, reason)
		VALUES ($1, $2, $3, $4, $5, $6)`, orgID, updated.UUID, eventType, updated.ResourceVersion, actor, strings.TrimSpace(reason)); err != nil {
		return nil, errors.Wrap(err, "failed to append module catalogue event")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.catalogue.changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldModuleUUID: updated.UUID, eventFieldModuleSlug: updated.Slug,
		"catalogue_status": updated.Status, eventFieldResourceVer: updated.ResourceVersion,
		eventFieldActor: actor, eventFieldReason: strings.TrimSpace(reason),
	}); err != nil {
		return nil, err
	}
	return updated, nil
}

func scanCoreModuleVersion(row interface{ Scan(...any) error }) (*CoreModuleVersion, error) {
	var result CoreModuleVersion
	var semanticVersion, sourceRevision, releaseNotes sql.NullString
	var publishedBy uuid.NullUUID
	err := row.Scan(&result.OrgID, &result.ModuleUUID, &result.ModuleSlug, &result.UUID, &semanticVersion,
		&result.OpaqueVersionID, &result.MigrationGeneration, &result.ArtifactDigest, &result.VerificationStatus,
		&result.LifecycleStatus, &sourceRevision, &releaseNotes, &result.ResourceVersion, &publishedBy, &result.CreatedAt)
	if err != nil {
		return nil, err
	}
	if semanticVersion.Valid {
		result.SemanticVersion = &semanticVersion.String
	}
	if sourceRevision.Valid {
		result.SourceRevision = sourceRevision.String
	}
	if releaseNotes.Valid {
		result.ReleaseNotes = &releaseNotes.String
	}
	if publishedBy.Valid {
		value := publishedBy.UUID
		result.PublishedBy = &value
	}
	return &result, nil
}

const coreModuleVersionColumns = `v.org_id, v.module_uuid, v.definition_id, v.uuid, v.semantic_version,
	v.version_id, v.migration_generation, COALESCE(v.artifact_digest, ''), v.verification_status,
	v.semantic_status, v.source_revision, v.release_notes, v.resource_version, v.published_by, v.created_at`

func (d *databaser) GetCoreModuleVersion(ctx context.Context, optionalTx Tx, orgID, moduleRef, versionRef string, mode GetMode) (*CoreModuleVersion, error) {
	version, err := scanCoreModuleVersion(d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT `+coreModuleVersionColumns+`
		FROM definition_versions v INNER JOIN definitions d ON d.uuid = v.module_uuid
		WHERE v.org_id = $1 AND (d.id = $2 OR d.uuid::text = $2)
		AND (v.uuid::text = $3 OR v.semantic_version = $3 OR v.version_id = $3)`+GetModeSuffix(mode), orgID, moduleRef, versionRef))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("module version not found")
	}
	return version, errors.Wrap(err, "failed to get module version")
}

func (d *databaser) ListCoreModuleVersions(ctx context.Context, optionalTx Tx, orgID, moduleRef string, includeDeprecated, includeDefective bool) ([]CoreModuleVersion, error) {
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT `+coreModuleVersionColumns+`
		FROM definition_versions v INNER JOIN definitions d ON d.uuid = v.module_uuid
		WHERE v.org_id = $1 AND (d.id = $2 OR d.uuid::text = $2)
		AND ($3 OR v.semantic_status <> 'deprecated') AND ($4 OR v.semantic_status <> 'defective')
		ORDER BY v.created_at DESC, v.uuid`, orgID, moduleRef, includeDeprecated, includeDefective)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list module versions")
	}
	defer func() { _ = rows.Close() }()
	result := make([]CoreModuleVersion, 0)
	for rows.Next() {
		version, err := scanCoreModuleVersion(rows)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan module version")
		}
		result = append(result, *version)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate module versions")
}

func (d *databaser) TransitionCoreModuleVersion(ctx context.Context, tx Tx, orgID, moduleRef, versionRef string, target moduleversions.LifecycleStatus, expectedVersion int64, actor uuid.UUID, reason string, correlationID *uuid.UUID) (*CoreModuleVersion, error) {
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
	version, err := d.GetCoreModuleVersion(ctx, tx, orgID, moduleRef, versionRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if version.ResourceVersion != expectedVersion {
		return nil, NewErrConflict("module version changed since it was read")
	}
	if version.SemanticVersion == nil {
		return nil, NewErrBadRequest("legacy v0 versions change lifecycle only through rollback restoration")
	}
	parsed, err := moduleversions.ParseVersion(*version.SemanticVersion)
	if err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if err := moduleversions.ValidateLifecycleTransition(version.LifecycleStatus, target, parsed); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if version.LifecycleStatus == target {
		return version, nil
	}
	if module.Status == moduleversions.CatalogueArchived && target != moduleversions.LifecycleDefective {
		return nil, NewErrConflict("archived modules allow only the defective safety transition")
	}
	from := version.LifecycleStatus
	if target == moduleversions.LifecycleDefault {
		if from == moduleversions.LifecycleProposed {
			lineageHead, err := d.managedDefaultLineageHead(ctx, tx, *module)
			if err != nil {
				return nil, err
			}
			if lineageHead != nil && lineageHead.SemanticVersion != nil {
				floor, err := moduleversions.ParseVersion(*lineageHead.SemanticVersion)
				if err != nil {
					return nil, errors.Wrap(err, "stored Default lineage head is invalid")
				}
				if err := moduleversions.ValidatePublicationVersion(parsed, &floor); err != nil {
					return nil, NewErrConflict("Proposed Module Version is no longer above the Default lineage head")
				}
			}
		}
		if from == moduleversions.LifecycleDeprecated {
			if module.PreviousDefaultVersionUUID == nil || *module.PreviousDefaultVersionUUID != version.UUID {
				return nil, NewErrConflict("only the immediately preceding deprecated default can be restored")
			}
		}
		if module.CurrentDefaultVersionUUID != nil {
			var previousResourceVersion int64
			if err := tx.QueryRowContext(ctx, `UPDATE definition_versions SET semantic_status = 'deprecated', resource_version = resource_version + 1
				WHERE uuid = $1 AND semantic_status = 'default' RETURNING resource_version`, *module.CurrentDefaultVersionUUID).Scan(&previousResourceVersion); err != nil {
				return nil, errors.Wrap(err, "failed to deprecate previous default")
			}
			previousPayload, _ := json.Marshal(map[string]any{"replaced_default_version_uuid": module.CurrentDefaultVersionUUID})
			if _, err := tx.ExecContext(ctx, `INSERT INTO module_version_lifecycle_events
				(org_id, module_uuid, version_uuid, from_status, to_status, version_resource_version, actor, reason, correlation_id, payload)
				VALUES ($1, $2, $3, 'default', 'deprecated', $4, $5, $6, $7, $8)`, orgID, module.UUID,
				*module.CurrentDefaultVersionUUID, previousResourceVersion, actor, strings.TrimSpace(reason), correlationID, previousPayload); err != nil {
				return nil, errors.Wrap(err, "failed to append previous Default lifecycle event")
			}
		}
		generation := module.ManagedDefaultGeneration
		migrationGeneration := version.MigrationGeneration
		if from == moduleversions.LifecycleProposed {
			generation++
			migrationGeneration = "managed"
			if generation == 1 {
				migrationGeneration = "v1"
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE definitions SET latest_version_id = $3,
			current_default_version_uuid = $4, previous_default_version_uuid = $5,
			managed_default_generation = $6, resource_version = resource_version + 1
			WHERE org_id = $1 AND uuid = $2`, orgID, module.UUID, version.OpaqueVersionID, version.UUID,
			module.CurrentDefaultVersionUUID, generation); err != nil {
			return nil, errors.Wrap(err, "failed to update default pointer")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE definition_versions SET semantic_status = $2,
			migration_generation = $3, resource_version = resource_version + 1 WHERE uuid = $1`, version.UUID, target, migrationGeneration); err != nil {
			return nil, errors.Wrap(err, "failed to promote module version")
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE definition_versions SET semantic_status = $2,
			resource_version = resource_version + 1 WHERE uuid = $1`, version.UUID, target); err != nil {
			return nil, errors.Wrap(err, "failed to change module version lifecycle")
		}
		if from == moduleversions.LifecycleDefault && target == moduleversions.LifecycleDefective {
			if _, err := tx.ExecContext(ctx, `UPDATE definitions SET latest_version_id = NULL,
				current_default_version_uuid = NULL, previous_default_version_uuid = $3,
				resource_version = resource_version + 1 WHERE org_id = $1 AND uuid = $2`, orgID, module.UUID, module.PreviousDefaultVersionUUID); err != nil {
				return nil, errors.Wrap(err, "failed to clear defective default pointer")
			}
		}
	}
	updated, err := d.GetCoreModuleVersion(ctx, tx, orgID, moduleRef, version.UUID.String(), GetModeDefault)
	if err != nil {
		return nil, err
	}
	payload := json.RawMessage(`{}`)
	if target == moduleversions.LifecycleDefault {
		payload, _ = json.Marshal(map[string]any{"replaced_default_version_uuid": module.CurrentDefaultVersionUUID})
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO module_version_lifecycle_events
		(org_id, module_uuid, version_uuid, from_status, to_status, version_resource_version, actor, reason, correlation_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, orgID, module.UUID, version.UUID, from,
		target, updated.ResourceVersion, actor, strings.TrimSpace(reason), correlationID, payload); err != nil {
		return nil, errors.Wrap(err, "failed to append lifecycle event")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.version.lifecycle-changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldModuleUUID: module.UUID, eventFieldModuleSlug: module.Slug, eventFieldVersionUUID: updated.UUID,
		eventFieldSemanticVer: updated.SemanticVersion, eventFieldFromStatus: from, eventFieldToStatus: target,
		eventFieldResourceVer: updated.ResourceVersion, eventFieldActor: actor, eventFieldReason: strings.TrimSpace(reason),
		"correlation_id": correlationID, "payload": payload,
	}); err != nil {
		return nil, err
	}
	return updated, nil
}

func (d *databaser) ListModuleLifecycleEvents(ctx context.Context, optionalTx Tx, orgID, moduleRef, versionRef string) ([]ModuleLifecycleEvent, error) {
	version, err := d.GetCoreModuleVersion(ctx, optionalTx, orgID, moduleRef, versionRef, GetModeDefault)
	if err != nil {
		return nil, err
	}
	rows, err := d.txOrDb(optionalTx).QueryContext(ctx, `SELECT sequence, id, module_uuid, version_uuid, from_status,
		to_status, version_resource_version, actor, reason, correlation_id, payload, created_at
		FROM module_version_lifecycle_events WHERE version_uuid = $1 ORDER BY sequence`, version.UUID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list lifecycle events")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleLifecycleEvent, 0)
	for rows.Next() {
		var item ModuleLifecycleEvent
		var fromStatus, reason sql.NullString
		var correlationID uuid.NullUUID
		if err := rows.Scan(&item.Sequence, &item.ID, &item.ModuleUUID, &item.VersionUUID, &fromStatus,
			&item.ToStatus, &item.ResourceVersion, &item.Actor, &reason, &correlationID, &item.Payload, &item.CreatedAt); err != nil {
			return nil, errors.Wrap(err, "failed to scan lifecycle event")
		}
		if fromStatus.Valid {
			value := moduleversions.LifecycleStatus(fromStatus.String)
			item.FromStatus = &value
		}
		if reason.Valid {
			item.Reason = &reason.String
		}
		if correlationID.Valid {
			value := correlationID.UUID
			item.CorrelationID = &value
		}
		result = append(result, item)
	}
	return result, errors.Wrap(rows.Err(), "failed to iterate lifecycle events")
}

func isUniqueViolation(err error, constraints ...string) bool {
	pqError := new(pq.Error)
	if !errors.As(err, &pqError) || pqError.Code.Name() != UniqueViolationErrorCode {
		return false
	}
	for _, constraint := range constraints {
		if pqError.Constraint == constraint {
			return true
		}
	}
	return false
}

func (d *databaser) GetModuleCoreCommand(ctx context.Context, optionalTx Tx, orgID, scope, key string) (string, json.RawMessage, bool, error) {
	if optionalTx != nil {
		// Serialize identical commands before checking for a result. The transaction
		// owns both the lock and every state/event write, including on rollback.
		lockIdentity, err := json.Marshal([]string{orgID, scope, key})
		if err != nil {
			return "", nil, false, errors.Wrap(err, "failed to encode module command lock")
		}
		if _, err := optionalTx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", string(lockIdentity)); err != nil {
			return "", nil, false, errors.Wrap(err, "failed to lock module command")
		}
	}
	var fingerprint string
	var response json.RawMessage
	err := d.txOrDb(optionalTx).QueryRowContext(ctx, `SELECT command_fingerprint, response FROM module_core_commands
		WHERE org_id = $1 AND command_scope = $2 AND idempotency_key = $3`, orgID, scope, key).Scan(&fingerprint, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	return fingerprint, response, err == nil, errors.Wrap(err, "failed to read module command")
}

func (d *databaser) StoreModuleCoreCommand(ctx context.Context, tx Tx, orgID, scope, key, fingerprint string, actor uuid.UUID, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return errors.Wrap(err, "failed to encode module command response")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO module_core_commands
		(org_id, command_scope, idempotency_key, command_fingerprint, actor, response)
		VALUES ($1, $2, $3, $4, $5, $6)`, orgID, scope, key, fingerprint, actor, encoded)
	if isUniqueViolation(err, "module_core_commands_pkey") {
		return NewErrConflict("idempotency key was used concurrently")
	}
	return errors.Wrap(err, "failed to store module command")
}
