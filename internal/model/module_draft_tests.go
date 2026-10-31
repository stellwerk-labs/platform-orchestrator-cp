package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleconformance"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

// ModuleDraftTestSnapshot is immutable executable material, not a published
// Module Version or evidence of a successful Deployment.
type ModuleDraftTestSnapshot struct {
	ID                      uuid.UUID
	OrgID                   string
	ModuleUUID              uuid.UUID
	ModuleSlug              string
	DraftID                 uuid.UUID
	DraftRevision           int64
	ResourceTypeVersionUUID uuid.UUID
	ResourceTypeVersion     int64
	ProjectUUID             uuid.UUID
	EnvironmentUUID         uuid.UUID
	Content                 json.RawMessage
	CapturedAt              time.Time
	CapturedBy              uuid.UUID
	Reason                  string
}

type ModuleDraftTestCapture struct {
	DraftID               uuid.UUID
	ExpectedDraftRevision int64
	EnvironmentUUID       uuid.UUID
	Actor                 uuid.UUID
	Reason                string
}

func (d *databaser) GetModuleDraftTestSnapshot(ctx context.Context, tx Tx, orgID, moduleRef string, environment, id uuid.UUID) (*ModuleDraftTestSnapshot, error) {
	var result ModuleDraftTestSnapshot
	err := d.txOrDb(tx).QueryRowContext(ctx, `SELECT s.id,s.org_id,s.module_uuid,m.id,s.draft_id,s.draft_revision,
  s.resource_type_version_uuid,v.version,s.project_uuid,s.environment_uuid,s.content,s.captured_at,s.captured_by,s.reason
  FROM module_draft_test_snapshots s JOIN definitions m ON m.uuid=s.module_uuid
  JOIN resource_type_versions v ON v.uuid=s.resource_type_version_uuid
  WHERE s.org_id=$1 AND (m.id=$2 OR m.uuid::text=$2) AND s.environment_uuid=$3 AND s.id=$4`, orgID, moduleRef, environment, id).
		Scan(&result.ID, &result.OrgID, &result.ModuleUUID, &result.ModuleSlug, &result.DraftID, &result.DraftRevision,
			&result.ResourceTypeVersionUUID, &result.ResourceTypeVersion, &result.ProjectUUID, &result.EnvironmentUUID, &result.Content, &result.CapturedAt, &result.CapturedBy, &result.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewErrNotFound("Module Draft test snapshot not found in this scope")
	}
	return &result, errors.Wrap(err, "failed to read Module Draft test snapshot")
}

// CaptureModuleDraftTestSnapshot requires the caller's authoring and deployment
// authorization. It captures material only; admission and execution are separate.
func (d *databaser) CaptureModuleDraftTestSnapshot(ctx context.Context, tx Tx, orgID, moduleRef string, request ModuleDraftTestCapture) (*ModuleDraftTestSnapshot, error) {
	if err := validateDraftCommand(tx, request.Actor, request.Reason, nil); err != nil {
		return nil, err
	}
	environment, err := d.GetEnvironmentByUuid(ctx, tx, orgID, request.EnvironmentUUID, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if environment.Status != EnvironmentStatusActive {
		return nil, NewErrConflict("Draft tests require an active Environment")
	}
	module, err := d.GetModuleCatalogue(ctx, tx, orgID, moduleRef, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if module.Status != moduleversions.CatalogueActive {
		return nil, NewErrConflict("archived Modules do not accept new Draft tests")
	}
	draft, err := d.GetModuleDraft(ctx, tx, orgID, moduleRef, request.DraftID, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if draft.Status != "open" || draft.ResourceVersion != request.ExpectedDraftRevision {
		return nil, NewErrConflict("Draft changed since review or is already closed")
	}
	// The API owns the authoring schema. This storage projection reads only the
	// fields needed to freeze source and exact interface identity; the complete
	// original JSON is retained unchanged.
	var body struct {
		SemanticVersion     string            `json:"semantic_version"`
		ModuleSource        string            `json:"module_source"`
		ModuleSourceCode    *string           `json:"module_source_code"`
		SourceRevision      *string           `json:"source_revision"`
		ArtifactDigest      *string           `json:"artifact_digest"`
		ResourceTypeVersion *int64            `json:"resource_type_version"`
		OutputSchema        *map[string]any   `json:"output_schema"`
		ProviderMapping     map[string]string `json:"provider_mapping"`
	}
	if err := json.Unmarshal(draft.Content, &body); err != nil {
		return nil, NewErrBadRequest("stored Draft definition is invalid")
	}
	if _, err := moduleversions.ParseVersion(body.SemanticVersion); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	if err := moduleversions.ValidateDraftTestSource(body.ModuleSource, body.ModuleSourceCode, body.SourceRevision, body.ArtifactDigest); err != nil {
		return nil, NewErrBadRequest(err.Error())
	}
	selected := int64(1)
	if body.ResourceTypeVersion != nil {
		selected = *body.ResourceTypeVersion
		if selected < 1 {
			return nil, NewErrBadRequest("resource_type_version must be positive")
		}
	}
	definition := ModuleDefinitionVersion{OrgId: orgID, DefinitionId: module.Slug, ResourceType: module.ResourceType, ResourceTypeVersion: selected}
	contract, err := d.resolveModuleResourceTypeContract(ctx, tx, &definition)
	if err != nil {
		return nil, err
	}
	var outputs map[string]any
	if body.OutputSchema != nil {
		outputs = map[string]any(*body.OutputSchema)
	}
	if err := moduleconformance.ValidateOutputs(contract.OutputsSchema, outputs); err != nil {
		return nil, conformanceBadRequest(err)
	}
	if contract.ModuleContract != nil {
		var document map[string]any
		if err := json.Unmarshal(draft.Content, &document); err != nil {
			return nil, err
		}
		bounded := map[string]any{}
		for _, key := range []string{"module_inputs", "module_params", "provider_mapping", "dependencies"} {
			value := document[key]
			if value == nil {
				value = map[string]any{}
			}
			bounded[key] = value
		}
		bounded["coprovisioned"] = document["coprovisioned"]
		if bounded["coprovisioned"] == nil {
			bounded["coprovisioned"] = []any{}
		}
		if outputs != nil {
			bounded["output_schema"] = outputs
		}
		if err := moduleconformance.ValidateDefinition(ctx, contract.ModuleContract, bounded); err != nil {
			return nil, conformanceBadRequest(err)
		}
	}
	if userErr, err := validateProviders(ctx, tx, orgID, body.ProviderMapping); err != nil {
		return nil, err
	} else if userErr != nil {
		return nil, NewErrBadRequest(userErr.Error())
	}
	var id uuid.UUID
	err = tx.QueryRowContext(ctx, `INSERT INTO module_draft_test_snapshots(org_id,module_uuid,draft_id,draft_revision,
  resource_type_version_uuid,project_uuid,environment_uuid,content,captured_by,reason)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, orgID, module.UUID, draft.ID, draft.ResourceVersion,
		definition.ResourceTypeVersionUUID, environment.ProjectUuid, environment.Uuid, draft.Content, request.Actor, strings.TrimSpace(request.Reason)).Scan(&id)
	if err != nil {
		return nil, errors.Wrap(err, "failed to capture Module Draft test snapshot")
	}
	if err := d.appendModuleCoreOutboxEvent(ctx, tx, "io.platform-orchestrator.module.draft.test.captured", map[string]any{
		eventFieldOrgID: orgID, eventFieldModuleUUID: module.UUID, "snapshot_uuid": id, "draft_uuid": draft.ID, "draft_revision": draft.ResourceVersion,
		"environment_uuid": environment.Uuid, "resource_type_version_uuid": definition.ResourceTypeVersionUUID, eventFieldActor: request.Actor, eventFieldReason: strings.TrimSpace(request.Reason),
	}); err != nil {
		return nil, err
	}
	return d.GetModuleDraftTestSnapshot(ctx, tx, orgID, moduleRef, environment.Uuid, id)
}
