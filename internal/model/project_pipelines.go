package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

// Pipeline transactions lock the project before authoring, policy changes or new admission.
// This makes policy selection and retirement serializable without a second scheduler.
func LockPipelineProject(ctx context.Context, db Databaser, tx Tx, org, project string) (*Project, error) {
	if tx == nil {
		return nil, errors.New("pipeline project transaction required")
	}
	value, err := db.GetProject(ctx, tx, org, project, GetModeForUpdate)
	if err != nil {
		return nil, err
	}
	if value.Status != ProjectStatusActive {
		return nil, NewErrConflict("project is not active")
	}
	return value, nil
}

const pipelineDraftColumns = `id,slug,name,resource_version,definition,updated_at,updated_by`

func scanPipelineDraft(row interface{ Scan(...any) error }, project string) (p.PipelineDraft, error) {
	var value p.PipelineDraft
	var raw []byte
	err := row.Scan(&value.Id, &value.Slug, &value.Name, &value.ResourceVersion, &raw, &value.UpdatedAt, &value.UpdatedBy)
	if err == nil {
		err = json.Unmarshal(raw, &value.Definition)
	}
	value.ProjectId = project
	return value, err
}
func ReadPipelineDraft(ctx context.Context, tx Tx, project *Project, id uuid.UUID) (p.PipelineDraft, error) {
	value, err := scanPipelineDraft(tx.QueryRowContext(ctx, `SELECT `+pipelineDraftColumns+` FROM project_pipeline_drafts WHERE project_uuid=$1 AND id=$2 AND archived_at IS NULL`, project.Uuid, id), project.Id)
	if errors.Is(err, sql.ErrNoRows) {
		return value, NewErrNotFound("pipeline draft not found in this project")
	}
	return value, err
}
func listPipelineDrafts(ctx context.Context, tx Tx, project *Project) ([]p.PipelineDraft, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pipelineDraftColumns+` FROM project_pipeline_drafts WHERE project_uuid=$1 AND archived_at IS NULL ORDER BY slug`, project.Uuid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []p.PipelineDraft{}
	for rows.Next() {
		value, err := scanPipelineDraft(rows, project.Id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func pipelineHistory(ctx context.Context, tx Tx, project uuid.UUID, kind string, subject *uuid.UUID, revision int64, actor uuid.UUID, content any) error {
	raw, err := json.Marshal(content)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_pipeline_history(project_uuid,kind,subject,resource_version,actor,content) VALUES($1,$2,$3,$4,$5,$6)`, project, kind, subject, revision, actor, raw)
	return err
}

func CreatePipelineDraft(ctx context.Context, tx Tx, project *Project, actor uuid.UUID, body p.PipelineDraftCreate) (p.PipelineDraft, error) {
	var zero p.PipelineDraft
	if actor == uuid.Nil {
		return zero, NewErrBadRequest("pipeline actor required")
	}
	if err := p.ValidateDraft(body.Slug, body.Name, body.Definition); err != nil {
		return zero, NewErrBadRequest(err.Error())
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM project_pipeline_drafts WHERE project_uuid=$1 AND archived_at IS NULL`, project.Uuid).Scan(&count); err != nil {
		return zero, err
	}
	if count >= 100 {
		return zero, NewErrConflict("project pipeline limit of 100 reached")
	}
	raw, _ := json.Marshal(body.Definition)
	id := uuid.New()
	_, err := tx.ExecContext(ctx, `INSERT INTO project_pipeline_drafts(id,project_uuid,slug,name,definition,updated_by) VALUES($1,$2,$3,$4,$5,$6)`, id, project.Uuid, body.Slug, body.Name, raw, actor)
	if pqerr, ok := err.(*pq.Error); ok && pqerr.Code == "23505" {
		return zero, NewErrConflict("pipeline slug already exists in this project")
	}
	if err != nil {
		return zero, err
	}
	value, err := ReadPipelineDraft(ctx, tx, project, id)
	if err != nil {
		return zero, err
	}
	err = pipelineHistory(ctx, tx, project.Uuid, "draft", &id, value.ResourceVersion, actor, value)
	return value, err
}

func SavePipelineDraft(ctx context.Context, tx Tx, project *Project, id, actor uuid.UUID, body p.PipelineDraftWrite) (p.PipelineDraft, error) {
	value, err := ReadPipelineDraft(ctx, tx, project, id)
	if err != nil {
		return value, err
	}
	if err := p.ValidateDraft(value.Slug, body.Name, body.Definition); err != nil {
		return value, NewErrBadRequest(err.Error())
	}
	if body.ExpectedResourceVersion != value.ResourceVersion {
		return value, NewErrConflict("pipeline draft changed; reload before saving")
	}
	raw, _ := json.Marshal(body.Definition)
	_, err = tx.ExecContext(ctx, `UPDATE project_pipeline_drafts SET name=$3,definition=$4,resource_version=resource_version+1,updated_at=now(),updated_by=$5 WHERE project_uuid=$1 AND id=$2`, project.Uuid, id, body.Name, raw, actor)
	if err != nil {
		return value, err
	}
	value, err = ReadPipelineDraft(ctx, tx, project, id)
	if err != nil {
		return value, err
	}
	err = pipelineHistory(ctx, tx, project.Uuid, "draft", &id, value.ResourceVersion, actor, value)
	return value, err
}

const publishedPipelineColumns = `id,pipeline_id,version,name,digest,definition,published_at,published_by,retired_at`

func scanPublishedPipeline(row interface{ Scan(...any) error }, project string) (p.PublishedPipelineVersion, error) {
	var value p.PublishedPipelineVersion
	var raw []byte
	err := row.Scan(&value.Id, &value.PipelineId, &value.Version, &value.Name, &value.Digest, &raw, &value.PublishedAt, &value.PublishedBy, &value.RetiredAt)
	if err == nil {
		err = json.Unmarshal(raw, &value.Definition)
	}
	value.ProjectId = project
	return value, err
}
func ReadPublishedPipeline(ctx context.Context, tx Tx, project *Project, id uuid.UUID) (p.PublishedPipelineVersion, error) {
	value, err := scanPublishedPipeline(tx.QueryRowContext(ctx, `SELECT `+publishedPipelineColumns+` FROM project_pipeline_versions WHERE project_uuid=$1 AND id=$2`, project.Uuid, id), project.Id)
	if errors.Is(err, sql.ErrNoRows) {
		return value, NewErrNotFound("published pipeline not found in this project")
	}
	return value, err
}

func PublishPipeline(ctx context.Context, tx Tx, project *Project, id, actor uuid.UUID, body p.PipelinePublish) (p.PublishedPipelineVersion, error) {
	var value p.PublishedPipelineVersion
	if len(body.RequestKey) < 1 || len(body.RequestKey) > 128 || strings.TrimSpace(body.RequestKey) == "" || body.ExpectedResourceVersion < 1 {
		return value, NewErrBadRequest("bounded request key and positive draft resource version required")
	}
	raw, _ := json.Marshal(struct {
		ID       uuid.UUID
		Revision int64
	}{id, body.ExpectedResourceVersion})
	hash := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(hash[:])
	if previous, found, err := recoverPipelinePublication(ctx, tx, project, actor, body.RequestKey, fingerprint); err != nil || found {
		return previous, err
	}
	draft, err := ReadPipelineDraft(ctx, tx, project, id)
	if err != nil {
		return value, err
	}
	if draft.ResourceVersion != body.ExpectedResourceVersion {
		return value, NewErrConflict("pipeline draft changed; review current revision before publishing")
	}
	value, err = publishPipelineRevision(ctx, tx, project, actor, draft)
	if err != nil {
		return value, err
	}
	response, _ := json.Marshal(value)
	_, err = tx.ExecContext(ctx, `INSERT INTO project_pipeline_publications(project_uuid,request_key,fingerprint,actor,response) VALUES($1,$2,$3,$4,$5)`, project.Uuid, body.RequestKey, fingerprint, actor, response)
	return value, err
}

func ReadPipelinePolicy(ctx context.Context, tx Tx, project uuid.UUID) (p.PipelinePolicy, error) {
	value := p.PipelinePolicy{AllowedVersionIds: []uuid.UUID{}, RollbackDefaults: &p.PipelineRollbackDefaults{}}
	var ids pq.StringArray
	var environmentDefaults, rollbackDefaults []byte
	err := tx.QueryRowContext(ctx, `SELECT resource_version,enabled,default_version_id,allowed_version_ids,updated_at,updated_by,environment_default_version_ids,rollback_defaults FROM project_pipeline_policy WHERE project_uuid=$1`, project).
		Scan(&value.ResourceVersion, &value.Enabled, &value.DefaultVersionId, &ids, &value.UpdatedAt, &value.UpdatedBy, &environmentDefaults, &rollbackDefaults)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(environmentDefaults, &value.EnvironmentDefaultVersionIds); err != nil {
		return value, err
	}
	if err := json.Unmarshal(rollbackDefaults, value.RollbackDefaults); err != nil {
		return value, err
	}
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return value, err
		}
		value.AllowedVersionIds = append(value.AllowedVersionIds, parsed)
	}
	return value, nil
}

func SetPipelinePolicy(ctx context.Context, tx Tx, project *Project, actor uuid.UUID, body p.PipelinePolicyWrite) (p.PipelinePolicy, error) {
	value, err := ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return value, err
	}
	if value.ResourceVersion != body.ExpectedResourceVersion {
		return value, NewErrConflict("project pipeline policy changed; reload before saving")
	}
	if body.RollbackDefaults == nil {
		body.RollbackDefaults = value.RollbackDefaults
	}
	ids, err := validatePipelineAllowedVersions(ctx, tx, project, body)
	if err != nil {
		return value, err
	}
	environmentDefaults, err := validateEnvironmentPipelineDefaults(ctx, tx, project, body)
	if err != nil {
		return value, err
	}
	rollbackDefaults, err := validateRollbackPipelineDefaults(ctx, tx, project, body)
	if err != nil {
		return value, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_pipeline_policy(project_uuid,resource_version,enabled,default_version_id,allowed_version_ids,updated_by,environment_default_version_ids,rollback_defaults) VALUES($1,1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(project_uuid) DO UPDATE SET resource_version=project_pipeline_policy.resource_version+1,enabled=EXCLUDED.enabled,default_version_id=EXCLUDED.default_version_id,allowed_version_ids=EXCLUDED.allowed_version_ids,updated_at=now(),updated_by=EXCLUDED.updated_by,environment_default_version_ids=EXCLUDED.environment_default_version_ids,rollback_defaults=EXCLUDED.rollback_defaults`, project.Uuid, body.Enabled, body.DefaultVersionId, pq.Array(ids), actor, environmentDefaults, rollbackDefaults)
	if err != nil {
		return value, err
	}
	value, err = ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return value, err
	}
	err = pipelineHistory(ctx, tx, project.Uuid, "policy", nil, value.ResourceVersion, actor, value)
	return value, err
}

func RetirePipeline(ctx context.Context, tx Tx, project *Project, id, actor uuid.UUID) (p.PublishedPipelineVersion, error) {
	value, err := ReadPublishedPipeline(ctx, tx, project, id)
	if err != nil {
		return value, err
	}
	if value.RetiredAt != nil {
		return value, nil
	}
	policy, err := ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return value, err
	}
	for _, allowed := range policy.AllowedVersionIds {
		if allowed == id {
			return value, NewErrConflict("remove this version from project policy before retirement")
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE project_pipeline_versions SET retired_at=now() WHERE project_uuid=$1 AND id=$2`, project.Uuid, id)
	if err != nil {
		return value, err
	}
	value, err = ReadPublishedPipeline(ctx, tx, project, id)
	if err != nil {
		return value, err
	}
	err = pipelineHistory(ctx, tx, project.Uuid, "retire", &id, int64(value.Version), actor, value)
	return value, err
}

func ListPipelineVersions(ctx context.Context, tx Tx, project *Project) ([]p.PublishedPipelineVersion, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+publishedPipelineColumns+` FROM project_pipeline_versions WHERE project_uuid=$1 ORDER BY published_at DESC,id DESC`, project.Uuid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := []p.PublishedPipelineVersion{}
	for rows.Next() {
		value, err := scanPublishedPipeline(rows, project.Id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func ReadPipelineCatalogue(ctx context.Context, tx Tx, project *Project) (p.PipelineCatalogue, error) {
	value := p.PipelineCatalogue{Drafts: []p.PipelineDraft{}, Versions: []p.PublishedPipelineVersion{}, Tools: p.TrustedTools()}
	var err error
	value.Drafts, err = listPipelineDrafts(ctx, tx, project)
	if err != nil {
		return value, err
	}
	value.Versions, err = ListPipelineVersions(ctx, tx, project)
	if err != nil {
		return value, err
	}
	value.Policy, err = ReadPipelinePolicy(ctx, tx, project.Uuid)
	return value, err
}

// CheckPipelineAdmission runs under the project lock in the ordinary Core admission transaction.
// Missing provenance on every legacy/internal path fails closed after policy is enabled.
func CheckPipelineAdmission(ctx context.Context, tx Tx, project *Project, reference *PipelineAdmissionReference) error {
	policy, err := ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return err
	}
	if reference == nil {
		if policy.Enabled {
			return NewErrConflict("project requires a published pipeline; this deployment path has no pipeline authority")
		}
		return nil
	}
	version, err := ReadPublishedPipeline(ctx, tx, project, reference.VersionID)
	if err != nil {
		return err
	}
	if reference.PipelineID != version.PipelineId || reference.Version != version.Version || reference.Digest != version.Digest {
		return NewErrBadRequest("pipeline provenance does not match immutable published version")
	}
	if version.RetiredAt != nil {
		return NewErrConflict("pipeline version is retired for new deployment use")
	}
	if policy.Enabled {
		for _, id := range policy.AllowedVersionIds {
			if id == version.Id {
				return nil
			}
		}
		return NewErrConflict("pipeline version is not allowed by current project policy")
	}
	return nil
}

type PipelineAdmissionReference struct {
	PipelineID uuid.UUID `json:"pipeline_id"`
	VersionID  uuid.UUID `json:"version_id"`
	Version    int       `json:"version"`
	Digest     string    `json:"digest"`
}

func recoverPipelinePublication(ctx context.Context, tx Tx, project *Project, actor uuid.UUID, key, fingerprint string) (p.PublishedPipelineVersion, bool, error) {
	var value p.PublishedPipelineVersion
	var previousFingerprint string
	var previousActor uuid.UUID
	var previous []byte
	err := tx.QueryRowContext(ctx, `SELECT fingerprint,actor,response FROM project_pipeline_publications WHERE project_uuid=$1 AND request_key=$2`, project.Uuid, key).Scan(&previousFingerprint, &previousActor, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if previousFingerprint != fingerprint || previousActor != actor {
		return value, false, NewErrConflict("publication request key belongs to another command")
	}
	err = json.Unmarshal(previous, &value)
	return value, true, err
}
func publishPipelineRevision(ctx context.Context, tx Tx, project *Project, actor uuid.UUID, draft p.PipelineDraft) (p.PublishedPipelineVersion, error) {
	value, err := scanPublishedPipeline(tx.QueryRowContext(ctx, `SELECT `+publishedPipelineColumns+` FROM project_pipeline_versions WHERE project_uuid=$1 AND pipeline_id=$2 AND draft_resource_version=$3`, project.Uuid, draft.Id, draft.ResourceVersion), project.Id)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return value, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM project_pipeline_versions WHERE project_uuid=$1`, project.Uuid).Scan(&count); err != nil {
		return value, err
	}
	if count >= 1000 {
		return value, NewErrConflict("project published pipeline limit of 1000 reached")
	}
	digest, err := p.RecipeDigest(draft.Name, draft.Definition)
	if err != nil {
		return value, NewErrBadRequest(err.Error())
	}
	definition, _ := json.Marshal(draft.Definition)
	value, err = scanPublishedPipeline(tx.QueryRowContext(ctx, `INSERT INTO project_pipeline_versions(project_uuid,pipeline_id,version,draft_resource_version,name,digest,definition,published_by)
 VALUES($1,$2,(SELECT COALESCE(max(version),0)+1 FROM project_pipeline_versions WHERE pipeline_id=$2),$3,$4,$5,$6,$7) RETURNING `+publishedPipelineColumns, project.Uuid, draft.Id, draft.ResourceVersion, draft.Name, digest, definition, actor), project.Id)
	if err != nil {
		return value, err
	}
	err = pipelineHistory(ctx, tx, project.Uuid, "publish", &value.Id, int64(value.Version), actor, value)
	return value, err
}

func validatePipelineAllowedVersions(ctx context.Context, tx Tx, project *Project, body p.PipelinePolicyWrite) ([]string, error) {
	if err := validatePipelinePolicyShape(body); err != nil {
		return nil, err
	}
	if len(body.AllowedVersionIds) > 100 {
		return nil, NewErrBadRequest("at most 100 allowed pipeline versions required")
	}
	seen := map[uuid.UUID]bool{}
	ids := []string{}
	for _, id := range body.AllowedVersionIds {
		if seen[id] || id == uuid.Nil {
			return nil, NewErrBadRequest("allowed versions must be unique immutable version IDs")
		}
		seen[id] = true
		version, err := ReadPublishedPipeline(ctx, tx, project, id)
		if err != nil {
			return nil, err
		}
		if version.RetiredAt != nil {
			return nil, NewErrConflict("retired pipeline versions cannot be allowed")
		}
		ids = append(ids, id.String())
	}

	return ids, nil
}

func validatePipelinePolicyShape(body p.PipelinePolicyWrite) error {
	if body.Enabled && (body.DefaultVersionId == nil || !slices.Contains(body.AllowedVersionIds, *body.DefaultVersionId)) {
		return NewErrBadRequest("enabled policy requires a default from allowed versions")
	}
	if !body.Enabled && (body.DefaultVersionId != nil || len(body.AllowedVersionIds) != 0 || len(body.EnvironmentDefaultVersionIds) > 0) {
		return NewErrBadRequest("disabled policy must have no default or allowed versions")
	}
	return nil
}

// ArchivePipelineDraft retains exact publication identities and immutable history.
// Publication retirement remains a separate, publisher-authorized operation.
func ArchivePipelineDraft(ctx context.Context, tx Tx, project *Project, id, actor uuid.UUID, expected int64) error {
	value, err := ReadPipelineDraft(ctx, tx, project, id)
	if err != nil {
		return err
	}
	if value.ResourceVersion != expected {
		return NewErrConflict("pipeline draft changed; reload before deleting")
	}
	var active, referenced bool
	err = tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM project_pipeline_versions WHERE project_uuid=$1 AND pipeline_id=$2 AND retired_at IS NULL),
 EXISTS(SELECT 1 FROM project_pipeline_policy p JOIN project_pipeline_versions v ON v.project_uuid=p.project_uuid
 WHERE p.project_uuid=$1 AND v.pipeline_id=$2 AND (v.id=p.default_version_id OR v.id=ANY(p.allowed_version_ids)))`, project.Uuid, id).Scan(&active, &referenced)
	if err != nil {
		return err
	}
	if referenced {
		return NewErrConflict("remove pipeline versions from project policy before deleting")
	}
	if active {
		return NewErrConflict("retire all published pipeline versions before deleting the draft")
	}
	_, err = tx.ExecContext(ctx, `UPDATE project_pipeline_drafts SET archived_at=now(),archived_by=$3,resource_version=resource_version+1,updated_at=now(),updated_by=$3 WHERE project_uuid=$1 AND id=$2`, project.Uuid, id, actor)
	if err != nil {
		return err
	}
	return pipelineHistory(ctx, tx, project.Uuid, "archive", &id, value.ResourceVersion+1, actor, value)
}

func validateEnvironmentPipelineDefaults(ctx context.Context, tx Tx, project *Project, body p.PipelinePolicyWrite) ([]byte, error) {
	if len(body.EnvironmentDefaultVersionIds) > 1000 {
		return nil, NewErrBadRequest("at most 1000 environment pipeline defaults")
	}
	values := map[string]uuid.UUID{}
	for key, version := range body.EnvironmentDefaultVersionIds {
		environment, err := uuid.Parse(key)
		if err != nil || environment == uuid.Nil || !slices.Contains(body.AllowedVersionIds, version) {
			return nil, NewErrBadRequest("environment default requires an allowed version and immutable environment identity")
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM envs WHERE uuid=$1 AND project_uuid=$2)`, environment, project.Uuid).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, NewErrBadRequest("pipeline default environment does not belong to project")
		}
		values[environment.String()] = version
	}
	return json.Marshal(values)
}
