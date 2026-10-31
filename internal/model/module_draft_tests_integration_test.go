//go:build integration

package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func draftTestContent(t *testing.T, source string) json.RawMessage {
	t.Helper()
	value, err := json.Marshal(map[string]any{"semantic_version": "1.2.0", "module_source": "inline", "module_source_code": source,
		"module_inputs": map[string]any{}, "module_params": map[string]any{}, "provider_mapping": map[string]any{}, "dependencies": map[string]any{}, "coprovisioned": []any{},
		"output_schema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}})
	require.NoError(t, err)
	return value
}

func TestModuleDraftTestSnapshotRetainsExactRevisionAndScope(t *testing.T) {
	ctx := t.Context()
	dsn := isolatedSchema(t)
	db, err := NewDatabaser(ctx, zap.NewNop(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	fixture := externalGrantFixture(t, db)
	environment := fixture.Intent.Environments[0].EnvironmentID
	contentA := draftTestContent(t, `output "name" { value = "A" }`)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err := db.CreateModuleDraft(ctx, tx, fixture.OrgID, "alpha", contentA, fixture.ActorID, "Begin testing")
	require.NoError(t, err)
	snapshotA, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: environment, Actor: fixture.ActorID, Reason: "Capture A"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, int64(1), snapshotA.ResourceTypeVersion)
	require.NotEqual(t, draft.ID, snapshotA.ID)
	require.Equal(t, fixture.Intent.Environments[0].ProjectID, snapshotA.ProjectUUID)
	_, err = db.GetCoreModuleVersion(ctx, nil, fixture.OrgID, "alpha", snapshotA.ID.String(), GetModeDefault)
	_, missing := IsErrNotFound(err)
	require.True(t, missing, "a test must never become a published or Rollout target")
	contentB := draftTestContent(t, `output "name" { value = "B" }`)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = db.UpdateModuleDraft(ctx, tx, fixture.OrgID, "alpha", draft.ID, 1, contentB, fixture.ActorID, "Try B with the same intended version")
	require.NoError(t, err)
	snapshotB, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 2, EnvironmentUUID: environment, Actor: fixture.ActorID, Reason: "Capture B"})
	require.NoError(t, err)
	_, err = db.CloseModuleDraft(ctx, tx, fixture.OrgID, "alpha", draft.ID, 2, nil, fixture.ActorID, "Close authoring")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NotEqual(t, snapshotA.ID, snapshotB.ID)
	retained, err := db.GetModuleDraftTestSnapshot(ctx, nil, fixture.OrgID, "alpha", environment, snapshotA.ID)
	require.NoError(t, err)
	require.JSONEq(t, string(contentA), string(retained.Content))
	for _, scope := range []struct {
		org, module string
		env         uuid.UUID
	}{{"foreign", "alpha", environment}, {fixture.OrgID, "bravo", environment}, {fixture.OrgID, "alpha", uuid.New()}} {
		_, err = db.GetModuleDraftTestSnapshot(ctx, nil, scope.org, scope.module, scope.env, snapshotA.ID)
		_, missing = IsErrNotFound(err)
		require.True(t, missing)
	}
	_, err = db.(*databaser).ExecContext(ctx, `UPDATE module_draft_test_snapshots SET content='{}' WHERE id=$1`, snapshotA.ID)
	require.ErrorContains(t, err, "snapshots are immutable")
	_, err = db.(*databaser).ExecContext(ctx, `DELETE FROM module_draft_test_snapshots WHERE id=$1`, snapshotA.ID)
	require.ErrorContains(t, err, "snapshots are immutable")
	_, err = extensionMigrationProvider(t, db).DownTo(ctx, 50)
	require.ErrorContains(t, err, "cannot downgrade while Draft test snapshots exist")
	require.NoError(t, db.Close())
	db, err = NewDatabaser(ctx, zap.NewNop(), dsn)
	require.NoError(t, err)
	restarted, err := db.GetModuleDraftTestSnapshot(ctx, nil, fixture.OrgID, "alpha", environment, snapshotA.ID)
	require.NoError(t, err)
	require.Equal(t, *retained, *restarted)
	require.NoError(t, db.DeleteEnvironment(ctx, nil, fixture.OrgID, "project", "development"))
	_, err = db.GetModuleDraftTestSnapshot(ctx, nil, fixture.OrgID, "alpha", environment, snapshotA.ID)
	require.NoError(t, err, "historical scope must not prevent normal Environment deletion")
}

func TestModuleDraftTestSnapshotRejectsStaleOrInvalidCapture(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	environment := fixture.Intent.Environments[0].EnvironmentID
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	draft, err := db.CreateModuleDraft(ctx, tx, fixture.OrgID, "alpha", draftTestContent(t, `output "name" { value = "A" }`), fixture.ActorID, "Start")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	request := ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: environment, Actor: fixture.ActorID, Reason: "Reviewed test"}
	for _, tt := range []struct {
		name   string
		change func(*ModuleDraftTestCapture)
	}{
		{"stale revision", func(r *ModuleDraftTestCapture) { r.ExpectedDraftRevision = 2 }},
		{"foreign scope", func(r *ModuleDraftTestCapture) { r.EnvironmentUUID = uuid.New() }},
		{"missing actor", func(r *ModuleDraftTestCapture) { r.Actor = uuid.Nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			changed := request
			tt.change(&changed)
			_, err = db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", changed)
			require.Error(t, err)
		})
	}
	for _, tt := range []struct{ name, content string }{
		{"moving source", `{"semantic_version":"1.2.0","module_source":"git::https://example.invalid/source?ref=main","source_revision":"` + strings.Repeat("a", 40) + `"}`},
		{"wrong output contract", `{"semantic_version":"1.2.0","module_source":"inline","module_source_code":"output {}","output_schema":{"type":"string"}}`},
		{"unknown interface", `{"semantic_version":"1.2.0","module_source":"inline","module_source_code":"output {}","resource_type_version":999}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = db.UpdateModuleDraft(ctx, tx, fixture.OrgID, "alpha", draft.ID, 1, json.RawMessage(tt.content), fixture.ActorID, "Try invalid candidate")
			require.NoError(t, err)
			changed := request
			changed.ExpectedDraftRevision = 2
			_, err = db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", changed)
			require.Error(t, err)
		})
	}
	var count int
	require.NoError(t, db.(*databaser).QueryRowContext(ctx, `SELECT count(*) FROM module_draft_test_snapshots`).Scan(&count))
	require.Zero(t, count)
	// Bypass the model once to prove database scope/content constraints remain authoritative.
	_, err = db.(*databaser).ExecContext(ctx, `INSERT INTO module_draft_test_snapshots(org_id,module_uuid,draft_id,draft_revision,resource_type_version_uuid,project_uuid,environment_uuid,content,captured_by,reason)
  SELECT d.org_id,d.module_uuid,d.id,d.resource_version,v.uuid,$2,$3,'{}',$4,'Forged snapshot'
  FROM module_drafts d JOIN definitions m ON m.uuid=d.module_uuid JOIN resource_type_versions v ON v.resource_type_uuid=m.resource_type_uuid AND v.version=1 WHERE d.id=$1`, draft.ID, fixture.Intent.Environments[0].ProjectID, environment, fixture.ActorID)
	require.ErrorContains(t, err, "snapshot must match")
}

func TestModuleDraftTestSnapshotBeforeFirstPublication(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	module, err := db.CreateEmptyModule(ctx, tx, fixture.OrgID, "first-attempt", "First attempt", "", "generic", map[string]string{})
	require.NoError(t, err)
	require.Nil(t, module.CurrentDefaultVersionUUID)
	draft, err := db.CreateModuleDraft(ctx, tx, fixture.OrgID, module.Slug, draftTestContent(t, `output "name" { value = "First" }`), fixture.ActorID, "Author the first version")
	require.NoError(t, err)
	snapshot, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, module.Slug, ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: fixture.Intent.Environments[0].EnvironmentID, Actor: fixture.ActorID, Reason: "Test before publishing anything"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, module.UUID, snapshot.ModuleUUID)
	current, err := db.GetModuleCatalogue(ctx, nil, fixture.OrgID, module.Slug, GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, current.CurrentDefaultVersionUUID)
	_, err = db.GetCoreModuleVersion(ctx, nil, fixture.OrgID, module.Slug, "1.2.0", GetModeDefault)
	_, missing := IsErrNotFound(err)
	require.True(t, missing)
}

func TestModuleDraftTestSnapshotSelectsExactInterface(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	output := map[string]any{"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}}
	version, err := db.PublishResourceTypeVersion(ctx, tx, fixture.OrgID, "generic", ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: output, Actor: fixture.ActorID, Reason: "Review endpoint interface"})
	require.NoError(t, err)
	var content map[string]any
	require.NoError(t, json.Unmarshal(draftTestContent(t, `output "endpoint" { value = "example" }`), &content))
	content["resource_type_version"] = 2
	content["output_schema"] = output
	raw, err := json.Marshal(content)
	require.NoError(t, err)
	draft, err := db.CreateModuleDraft(ctx, tx, fixture.OrgID, "alpha", raw, fixture.ActorID, "Use interface two")
	require.NoError(t, err)
	snapshot, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, fixture.OrgID, "alpha", ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: fixture.Intent.Environments[0].EnvironmentID, Actor: fixture.ActorID, Reason: "Test exact interface"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, version.UUID, snapshot.ResourceTypeVersionUUID)
	require.Equal(t, int64(2), snapshot.ResourceTypeVersion)
	retained, err := db.GetModuleDraftTestSnapshot(ctx, nil, fixture.OrgID, "alpha", snapshot.EnvironmentUUID, snapshot.ID)
	require.NoError(t, err)
	require.Equal(t, snapshot.ResourceTypeVersionUUID, retained.ResourceTypeVersionUUID)
	require.JSONEq(t, string(raw), string(retained.Content))
}
