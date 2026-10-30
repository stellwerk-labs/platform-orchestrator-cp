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

const maxModuleDraftBytes = 2 * 1024 * 1024

type ModuleDraft struct {
	ID                   uuid.UUID
	OrgID                string
	ModuleUUID           uuid.UUID
	ModuleSlug           string
	ResourceVersion      int64
	Content              json.RawMessage
	Status               string
	PublishedVersionUUID *uuid.UUID
	CreatedAt            time.Time
	CreatedBy            uuid.UUID
	UpdatedAt            time.Time
	UpdatedBy            uuid.UUID
}

type ModuleDraftRevision struct {
	DraftID              uuid.UUID
	ResourceVersion      int64
	Content              json.RawMessage
	Status               string
	PublishedVersionUUID *uuid.UUID
	CreatedAt            time.Time
	Actor                uuid.UUID
	Reason               string
}

const moduleDraftColumns = `d.id, d.org_id, d.module_uuid, m.id, d.resource_version,
	d.content, d.status, d.published_version_uuid, d.created_at, d.created_by, d.updated_at, d.updated_by`

func scanModuleDraft(row interface{ Scan(...any) error }) (*ModuleDraft, error) {
	var result ModuleDraft
	err := row.Scan(&result.ID, &result.OrgID, &result.ModuleUUID, &result.ModuleSlug,
		&result.ResourceVersion, &result.Content, &result.Status, &result.PublishedVersionUUID,
		&result.CreatedAt, &result.CreatedBy, &result.UpdatedAt, &result.UpdatedBy)
	return &result, err
}

func validateDraftCommand(tx Tx, actor uuid.UUID, reason string, content json.RawMessage) error {
	if tx == nil {
		return errors.New("transaction required")
	}
	if actor == uuid.Nil || strings.TrimSpace(reason) == "" || len(reason) > 2000 {
		return NewErrBadRequest("actor and non-empty reason of at most 2000 bytes are required")
	}
	if content != nil {
		var object map[string]json.RawMessage
		if len(content) > maxModuleDraftBytes || json.Unmarshal(content, &object) != nil || object == nil {
			return NewErrBadRequest("Draft content must be a JSON object of at most 2 MiB")
		}
	}
	return nil
}

func (d *databaser) GetModuleDraft(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, mode GetMode) (*ModuleDraft, error) {
	query := `SELECT ` + moduleDraftColumns + ` FROM module_drafts d JOIN definitions m ON m.uuid=d.module_uuid
		WHERE d.org_id=$1 AND (m.id=$2 OR m.uuid::text=$2) AND d.id=$3`
	if mode == GetModeForUpdate {
		if tx == nil {
			return nil, errors.New("transaction required for locked Draft read")
		}
		query += ` FOR UPDATE OF d`
	}
	draft, err := scanModuleDraft(d.txOrDb(tx).QueryRowContext(ctx, query, orgID, moduleRef, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("Module Draft not found")
	}
	return draft, errors.Wrap(err, "failed to read Module Draft")
}

func (d *databaser) GetModuleDraftRevision(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, revision int64) (*ModuleDraftRevision, error) {
	if _, err := d.GetModuleDraft(ctx, tx, orgID, moduleRef, id, GetModeDefault); err != nil {
		return nil, err
	}
	var result ModuleDraftRevision
	err := d.txOrDb(tx).QueryRowContext(ctx, `SELECT draft_id,resource_version,content,status,published_version_uuid,created_at,actor,reason
		FROM module_draft_revisions WHERE draft_id=$1 AND resource_version=$2`, id, revision).
		Scan(&result.DraftID, &result.ResourceVersion, &result.Content, &result.Status, &result.PublishedVersionUUID, &result.CreatedAt, &result.Actor, &result.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("Module Draft revision not found")
	}
	return &result, errors.Wrap(err, "failed to read Module Draft revision")
}

func (d *databaser) ListModuleDrafts(ctx context.Context, tx Tx, orgID, moduleRef string, after uuid.UUID, limit int) ([]ModuleDraft, error) {
	if limit < 1 || limit > 101 {
		return nil, NewErrBadRequest("invalid Module Draft page size")
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeDefault)
	if err != nil {
		return nil, err
	}
	rows, err := d.txOrDb(tx).QueryContext(ctx, `SELECT `+moduleDraftColumns+` FROM module_drafts d
		JOIN definitions m ON m.uuid=d.module_uuid WHERE d.org_id=$1 AND d.module_uuid=$2 AND d.id>$3 ORDER BY d.id LIMIT $4`, orgID, module.UUID, after, limit)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list Module Drafts")
	}
	defer func() { _ = rows.Close() }()
	result := make([]ModuleDraft, 0)
	for rows.Next() {
		draft, err := scanModuleDraft(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *draft)
	}
	return result, rows.Err()
}

func (d *databaser) CreateModuleDraft(ctx context.Context, tx Tx, orgID, moduleRef string, content json.RawMessage, actor uuid.UUID, reason string) (*ModuleDraft, error) {
	if err := validateDraftCommand(tx, actor, reason, content); err != nil {
		return nil, err
	}
	if content == nil {
		return nil, NewErrBadRequest("Draft content is required")
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if module.Status != moduleversions.CatalogueActive {
		return nil, NewErrConflict("archived Modules do not accept Draft authoring")
	}
	var id uuid.UUID
	err = tx.QueryRowContext(ctx, `INSERT INTO module_drafts(org_id,module_uuid,resource_version,content,status,created_by,updated_by)
		VALUES($1,$2,1,$3,'open',$4,$4) RETURNING id`, orgID, module.UUID, content, actor).Scan(&id)
	if isUniqueViolation(err, "module_drafts_one_open") {
		return nil, NewErrConflict("Module already has an open Draft")
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Module Draft")
	}
	return d.recordModuleDraftRevision(ctx, tx, orgID, moduleRef, id, actor, reason)
}

func (d *databaser) UpdateModuleDraft(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, expected int64, content json.RawMessage, actor uuid.UUID, reason string) (*ModuleDraft, error) {
	if content == nil {
		return nil, NewErrBadRequest("Draft content is required")
	}
	return d.changeModuleDraft(ctx, tx, orgID, moduleRef, id, expected, content, "open", nil, actor, reason)
}

func (d *databaser) CloseModuleDraft(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, expected int64, published *uuid.UUID, actor uuid.UUID, reason string) (*ModuleDraft, error) {
	status := "discarded"
	if published != nil {
		status = "published"
	}
	return d.changeModuleDraft(ctx, tx, orgID, moduleRef, id, expected, nil, status, published, actor, reason)
}

func (d *databaser) changeModuleDraft(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, expected int64, content json.RawMessage, status string, published *uuid.UUID, actor uuid.UUID, reason string) (*ModuleDraft, error) {
	if err := validateDraftCommand(tx, actor, reason, content); err != nil {
		return nil, err
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	current, err := d.GetModuleDraft(ctx, tx, orgID, moduleRef, id, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if current.Status != "open" || current.ResourceVersion != expected {
		return nil, NewErrConflict("Draft changed since it was read or is already closed")
	}
	if module.Status != moduleversions.CatalogueActive && status != "discarded" {
		return nil, NewErrConflict("archived Modules do not accept Draft authoring")
	}
	if content == nil {
		content = current.Content
	}
	_, err = tx.ExecContext(ctx, `UPDATE module_drafts SET content=$2,status=$3,published_version_uuid=$4,
		resource_version=resource_version+1,updated_at=now(),updated_by=$5 WHERE id=$1`, id, content, status, published, actor)
	if err != nil {
		return nil, errors.Wrap(err, "failed to change Module Draft")
	}
	return d.recordModuleDraftRevision(ctx, tx, orgID, moduleRef, id, actor, reason)
}

func (d *databaser) recordModuleDraftRevision(ctx context.Context, tx Tx, orgID, moduleRef string, id uuid.UUID, actor uuid.UUID, reason string) (*ModuleDraft, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO module_draft_revisions(draft_id,resource_version,content,status,published_version_uuid,actor,reason)
		SELECT id,resource_version,content,status,published_version_uuid,$2,$3 FROM module_drafts WHERE id=$1`, id, actor, strings.TrimSpace(reason))
	if err != nil {
		return nil, errors.Wrap(err, "failed to record immutable Draft revision")
	}
	result, err := d.GetModuleDraft(ctx, tx, orgID, moduleRef, id, GetModeDefault)
	if err != nil {
		return nil, err
	}
	err = d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.draft.changed", map[string]any{
		eventFieldOrgID: orgID, eventFieldModuleUUID: result.ModuleUUID, "draft_id": id, eventFieldResourceVer: result.ResourceVersion,
		"status": result.Status, eventFieldActor: actor, eventFieldReason: strings.TrimSpace(reason),
	})
	return result, err
}
