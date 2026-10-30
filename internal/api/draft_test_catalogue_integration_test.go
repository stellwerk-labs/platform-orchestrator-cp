//go:build integration

package api

import (
	"context"
	"encoding/json"
	"github.com/stellwerk-labs/golib/hecho"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func draftCatalogueFixture(t *testing.T, first bool) (*Server, *model.ModuleCatalogue, *model.ModuleDraft, ModuleDraftTestSnapshot, GenerateInternalExecutionCatalogueRequestObject) {
	t.Helper()
	ctx := t.Context()
	db := pluginTestDatabase(t)
	grant := seedPluginOperation(t, db, "draft-catalogue-"+uuid.NewString())
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	slug := "module"
	if first {
		slug = "first-attempt"
		_, err = db.CreateEmptyModule(ctx, tx, grant.OrgID, slug, "First attempt", "", "generic", map[string]string{})
		require.NoError(t, err)
	}
	module, err := db.GetModuleCatalogue(ctx, tx, grant.OrgID, slug, model.GetModeDefault)
	require.NoError(t, err)
	baseline, err := db.GetModuleDefinitionVersion(ctx, tx, grant.OrgID, "module", "1.0.0")
	require.NoError(t, err)
	body := ModuleVersionPublishBody{SemanticVersion: "1.2.0", ModuleSource: "inline", ModuleSourceCode: ref.Ref(`output "name" { value = "A" }`), ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParamItem{}, ProviderMapping: map[string]string{}, Dependencies: map[string]ModuleDependencyManifest{}, Coprovisioned: []ModuleCoProvisionManifest{}, OutputSchema: (*ModuleOutputSchema)(&baseline.OutputSchema)}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	draft, err := db.CreateModuleDraft(ctx, tx, grant.OrgID, slug, raw, grant.ActorID, "Author first implementation")
	require.NoError(t, err)
	snapshot, err := db.CaptureModuleDraftTestSnapshot(ctx, tx, grant.OrgID, slug, model.ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 1, EnvironmentUUID: grant.Intent.Environments[0].EnvironmentID, Actor: grant.ActorID, Reason: "Review exact test"})
	require.NoError(t, err)
	_, err = db.CreateModuleRule(ctx, tx, grant.OrgID, &model.DefinitionRule{OrgId: grant.OrgID, DefinitionId: slug, ResourceType: "generic", ResourceClass: "default"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	selected := map[string]uuid.UUID{module.UUID.String(): snapshot.ID}
	apiSnapshot, err := moduleDraftTestSnapshotToAPI(snapshot)
	require.NoError(t, err)
	request := GenerateInternalExecutionCatalogueRequestObject{OrgId: grant.OrgID, ProjectId: "project", EnvId: "development", Body: &InternalExecutionCatalogueGenerateBody{DraftTestSnapshots: &selected}}
	return &Server{Database: db, Logger: zap.NewNop()}, module, draft, apiSnapshot, request
}

func TestDraftTestCataloguePlannerPreservesFrozenMaterialBeforeFirstPublication(t *testing.T) {
	server, module, draft, snapshot, request := draftCatalogueFixture(t, true)
	require.Nil(t, module.CurrentDefaultVersionUUID)
	// Exercise the real planner/database separately from the authenticated RPC
	// boundary. Human IAM qualification is part of the full-stack suite.
	response, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	catalogue, ok := response.(GenerateInternalExecutionCatalogue200JSONResponse)
	require.True(t, ok, "unexpected response %T", response)
	require.Len(t, catalogue.Modules, 1)
	entry := catalogue.Modules[0]
	require.Equal(t, snapshot.Id, entry.VersionUuid)
	require.Equal(t, snapshot.Id.String(), entry.VersionId)
	require.Len(t, entry.Rules, 1)
	require.NotNil(t, entry.DraftTest)
	require.Nil(t, entry.SemanticStatus)
	require.Nil(t, entry.SemanticVersion)
	require.Nil(t, entry.MigrationGeneration)
	require.Nil(t, entry.VerificationStatus)
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	for _, field := range []string{"semantic_status", "semantic_version", "migration_generation", "verification_status"} {
		require.NotContains(t, wire, field)
	}
	require.Equal(t, snapshot.Definition.ModuleSourceCode, entry.ModuleSourceCode)
	changed := snapshot.Definition
	changed.ModuleSourceCode = ref.Ref(`output "name" { value = "B" }`)
	newContent, err := json.Marshal(changed)
	require.NoError(t, err)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = server.Database.UpdateModuleDraft(t.Context(), tx, request.OrgId, module.Slug, draft.ID, 1, newContent, snapshot.CapturedBy, "Try B under the same intended SemVer")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	response, err = server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	retained := response.(GenerateInternalExecutionCatalogue200JSONResponse).Modules[0]
	require.Equal(t, entry, retained)
	ordinary := request
	ordinary.Body = &InternalExecutionCatalogueGenerateBody{}
	response, err = server.generateInternalExecutionCatalogue(t.Context(), ordinary)
	require.Error(t, err, "ordinary Rules cannot adopt a Draft or invent the missing Default")
	require.Nil(t, response)
}

func TestDraftTestCataloguePlannerRejectsPinBypassAndPublicSpoof(t *testing.T) {
	server, module, _, snapshot, request := draftCatalogueFixture(t, false)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = server.Database.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgId, snapshot.ProjectUuid, "project", snapshot.EnvironmentUuid, "development", module.UUID, *module.CurrentDefaultVersionUUID, snapshot.CapturedBy, "user", "Protect published baseline", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	request.Params.XStellwerkModuleOperationPlanning = ref.Ref(true)
	response, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	require.IsType(t, GenerateInternalExecutionCatalogue409JSONResponse{}, response)
	_, err = server.GenerateInternalExecutionCatalogue(context.WithValue(t.Context(), hecho.ContextKeyUserID, uuid.NewString()), request)
	require.Error(t, err, "public callers cannot spoof the Data Plane test catalogue boundary")
}

func TestDraftCatalogueRetainsAWhileBReceivesRules(t *testing.T) {
	server, module, draft, a, request := draftCatalogueFixture(t, true)
	body := a.Definition
	body.ModuleSourceCode = ref.Ref(`output "name" { value = "B" }`)
	content, err := json.Marshal(body)
	require.NoError(t, err)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = server.Database.UpdateModuleDraft(t.Context(), tx, request.OrgId, module.Slug, draft.ID, 1, content, a.CapturedBy, "Try B")
	require.NoError(t, err)
	b, err := server.Database.CaptureModuleDraftTestSnapshot(t.Context(), tx, request.OrgId, module.Slug, model.ModuleDraftTestCapture{DraftID: draft.ID, ExpectedDraftRevision: 2, EnvironmentUUID: a.EnvironmentUuid, Actor: a.CapturedBy, Reason: "Review B"})
	require.NoError(t, err)
	_, err = server.Database.CloseModuleDraft(t.Context(), tx, request.OrgId, module.Slug, draft.ID, 2, nil, a.CapturedBy, "Retain captured tests")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	selected := map[string]uuid.UUID{module.UUID.String(): b.ID}
	request.Body.DraftTestSnapshots = &selected
	request.Body.RetainedDraftTestSnapshots = []ModuleDraftTestCoordinate{{ModuleUuid: module.UUID, SnapshotUuid: a.Id}, {ModuleUuid: module.UUID, SnapshotUuid: a.Id}}
	request.Body.PinnedModuleVersions = []string{module.Slug + "@" + a.Id.String()}
	result, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	catalogue := result.(GenerateInternalExecutionCatalogue200JSONResponse)
	require.Len(t, catalogue.Modules, 2)
	for _, entry := range catalogue.Modules {
		require.Equal(t, "1.2.0", entry.DraftTest.Definition.SemanticVersion)
		if entry.VersionUuid == a.Id {
			require.Empty(t, entry.Rules)
			require.Equal(t, a.Definition.ModuleSourceCode, entry.ModuleSourceCode)
		} else {
			require.Equal(t, b.ID, entry.VersionUuid)
			require.Len(t, entry.Rules, 1)
			require.Equal(t, body.ModuleSourceCode, entry.ModuleSourceCode)
		}
	}
}

func TestDraftCatalogueMaterialReadPreservesSourceAfterPinChanges(t *testing.T) {
	server, module, _, snapshot, request := draftCatalogueFixture(t, false)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = server.Database.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgId, snapshot.ProjectUuid, "project", snapshot.EnvironmentUuid, "development", module.UUID, *module.CurrentDefaultVersionUUID, snapshot.CapturedBy, "user", "Protect current published state", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	request.Body = &InternalExecutionCatalogueGenerateBody{MaterialOnly: true, AreRulesIgnored: true, RetainedDraftTestSnapshots: []ModuleDraftTestCoordinate{{ModuleUuid: module.UUID, SnapshotUuid: snapshot.Id}}}
	response, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	modules := response.(GenerateInternalExecutionCatalogue200JSONResponse).Modules
	require.Len(t, modules, 1)
	require.Equal(t, snapshot.Id, modules[0].VersionUuid)
	require.Empty(t, modules[0].Rules)
	request.Body.DraftTestSnapshots = &map[string]uuid.UUID{module.UUID.String(): snapshot.Id}
	_, err = server.generateInternalExecutionCatalogue(t.Context(), request)
	require.Error(t, err, "material reads cannot request fresh rule selection")
	request.Body.DraftTestSnapshots = nil
	request.EnvId = "foreign"
	response, err = server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	require.IsType(t, GenerateInternalExecutionCatalogue404JSONResponse{}, response)
}

func TestExecutionCatalogueSelectsFirstPublishedProposedWithoutDefault(t *testing.T) {
	server, module, _, snapshot, request := draftCatalogueFixture(t, true)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	definition := publicationModel(module, snapshot.Definition, snapshot.CapturedBy)
	published, err := server.Database.PublishCoreModuleVersion(t.Context(), tx, &definition, snapshot.CapturedBy)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	selection := map[string]string{module.Slug: published.UUID.String()}
	request.Body = &InternalExecutionCatalogueGenerateBody{SelectedPublishedModuleVersions: &selection}
	response, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	modules := response.(GenerateInternalExecutionCatalogue200JSONResponse).Modules
	require.Len(t, modules, 1)
	require.Equal(t, published.UUID, modules[0].VersionUuid)
	require.Len(t, modules[0].Rules, 1)
	require.Nil(t, modules[0].DraftTest)
}

func TestExecutionCatalogueResolvesPinnedFirstProposedBeforeDefaultLookup(t *testing.T) {
	server, module, _, snapshot, request := draftCatalogueFixture(t, true)
	tx, err := server.Database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	definition := publicationModel(module, snapshot.Definition, snapshot.CapturedBy)
	published, err := server.Database.PublishCoreModuleVersion(t.Context(), tx, &definition, snapshot.CapturedBy)
	require.NoError(t, err)
	_, err = server.Database.CreateEnvironmentModuleVersionPin(t.Context(), tx, request.OrgId, snapshot.ProjectUuid, "project", snapshot.EnvironmentUuid, "development", module.UUID, published.UUID, snapshot.CapturedBy, "user", "Protect exact first Proposed", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	request.Body = &InternalExecutionCatalogueGenerateBody{}
	response, err := server.generateInternalExecutionCatalogue(t.Context(), request)
	require.NoError(t, err)
	modules := response.(GenerateInternalExecutionCatalogue200JSONResponse).Modules
	require.Len(t, modules, 1)
	require.Equal(t, published.UUID, modules[0].VersionUuid)
	require.Len(t, modules[0].Rules, 1)
	require.Nil(t, modules[0].DraftTest)
	retained, err := server.Database.GetModuleCatalogue(t.Context(), nil, request.OrgId, module.Slug, model.GetModeDefault)
	require.NoError(t, err)
	require.Nil(t, retained.CurrentDefaultVersionUUID, "Pin selection must not manufacture a Default")
}
