package integrationtests

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
)

func TestEmptyModuleHardDeleteReleasesCreateIdempotency(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	resourceType := MustCreateResourceType(t, client, orgID, "recreate-"+strings.ToLower(rand.Text()))
	moduleSlug := "recreate-" + strings.ToLower(rand.Text())
	body := genclient.ModuleCatalogueCreateBody{Slug: moduleSlug, ResourceType: resourceType.Id}
	params := &genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: "stable-create-key"}

	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID, params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	require.NotNil(t, created.JSON201)

	deleted, err := client.DeleteModuleWithResponse(t.Context(), orgID, moduleSlug)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, deleted.StatusCode(), string(deleted.Body))

	recreated, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID, params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, recreated.StatusCode(), string(recreated.Body))
	require.NotNil(t, recreated.JSON201)
	assert.NotEqual(t, created.JSON201.Uuid, recreated.JSON201.Uuid)

	current, err := client.GetModuleCatalogueEntryWithResponse(t.Context(), orgID, moduleSlug)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, current.StatusCode(), string(current.Body))
	require.NotNil(t, current.JSON200)
	assert.Equal(t, recreated.JSON201.Uuid, current.JSON200.Uuid)
}

func TestModuleVersionManagementLifecycleAPI(t *testing.T) {
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgID := MustCreateOrg(t, internalClient).Id
	resourceType := MustCreateResourceType(t, client, orgID, "core-"+strings.ToLower(rand.Text()))
	moduleSlug := "release-" + strings.ToLower(rand.Text())
	environmentType := MustCreateEnvType(t, client, orgID, "core-test")
	project := MustCreateProject(t, client, orgID, "core-test")
	environment := MustCreateEnv(t, client, orgID, environmentType.Id, project.Id, "deletion-preview")

	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: "create-module-identity"},
		genclient.ModuleCatalogueCreateBody{Slug: moduleSlug, DisplayName: ptr("Payments Database"), Description: ptr("Shared payments database"), ResourceType: resourceType.Id})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	require.NotNil(t, created.JSON201)
	assert.Equal(t, genclient.ModuleCatalogueStatus("active"), created.JSON201.Status)
	assert.Nil(t, created.JSON201.CurrentDefaultVersionUuid)
	assert.EqualValues(t, 1, created.JSON201.ResourceVersion)

	operationID := uuid.New()
	reservation, err := internalClient.AcquireModuleOperationReservationWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.AcquireModuleOperationReservationParams{IdempotencyKey: "reserve-module-operation"},
		genclient.ModuleOperationReservationAcquireBody{Namespace: "dev.example.change-orchestrator", OperationId: operationID,
			RelatedResourceId: "change/video-release", Reason: "Freeze catalogue identity while planning the change"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, reservation.StatusCode(), string(reservation.Body))
	require.NotNil(t, reservation.JSON201)

	contribution, err := internalClient.UpsertModuleExtensionContributionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.UpsertModuleExtensionContributionParams{IdempotencyKey: "register-draft-change"},
		genclient.ModuleExtensionContributionUpsertBody{Namespace: "dev.example.change-orchestrator", ExternalResourceId: "change/video-release",
			EnvironmentUuid: &environment.Uuid, Kind: "related_resource", LifecycleState: "draft",
			Label: "Video release", Payload: map[string]any{"state": "draft"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, contribution.StatusCode(), string(contribution.Body))

	impact, err := client.GetEnvironmentDeletionImpactWithResponse(t.Context(), orgID, project.Id, environment.Id)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, impact.StatusCode(), string(impact.Body))
	require.NotNil(t, impact.JSON200)
	require.False(t, impact.JSON200.Blocked)
	require.Len(t, impact.JSON200.RelatedResources, 1)
	assert.Equal(t, environment.Uuid, *impact.JSON200.RelatedResources[0].EnvironmentUuid)

	blockedArchive, err := client.ChangeModuleCatalogueStatusWithResponse(t.Context(), orgID, moduleSlug,
		genclient.ChangeModuleCatalogueStatusParamsCatalogueActionArchive,
		&genclient.ChangeModuleCatalogueStatusParams{IdempotencyKey: "archive-blocked-by-reservation"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: created.JSON201.ResourceVersion, Reason: "Validate explicit blockers"})
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, blockedArchive.StatusCode(), string(blockedArchive.Body))
	require.Contains(t, string(blockedArchive.Body), "dev.example.change-orchestrator/change/video-release")

	released, err := internalClient.ReleaseModuleOperationReservationWithResponse(t.Context(), orgID, moduleSlug, reservation.JSON201.Id,
		&genclient.ReleaseModuleOperationReservationParams{IdempotencyKey: "release-module-operation"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: reservation.JSON201.ResourceVersion, Reason: "Planning finished"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, released.StatusCode(), string(released.Body))

	archived, err := client.ChangeModuleCatalogueStatusWithResponse(t.Context(), orgID, moduleSlug,
		genclient.ChangeModuleCatalogueStatusParamsCatalogueActionArchive,
		&genclient.ChangeModuleCatalogueStatusParams{IdempotencyKey: "archive-after-release"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: created.JSON201.ResourceVersion, Reason: "Confirm Draft links do not block archive"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, archived.StatusCode(), string(archived.Body))

	unarchived, err := client.ChangeModuleCatalogueStatusWithResponse(t.Context(), orgID, moduleSlug,
		genclient.ChangeModuleCatalogueStatusParamsCatalogueActionUnarchive,
		&genclient.ChangeModuleCatalogueStatusParams{IdempotencyKey: "unarchive-for-publication"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: archived.JSON200.ResourceVersion, Reason: "Continue lifecycle test"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, unarchived.StatusCode(), string(unarchived.Body))

	publishBody := genclient.ModuleVersionPublishBody{
		SemanticVersion: "1.0.0", ModuleSource: "inline", ModuleSourceCode: ptr("output \"name\" { value = \"payments\" }"),
		ModuleParams: map[string]genclient.ModuleParamItem{}, ModuleInputs: map[string]interface{}{},
		ProviderMapping: map[string]string{}, Dependencies: map[string]genclient.ModuleDependencyManifest{},
		Coprovisioned: []genclient.ModuleCoProvisionManifest{}, ReleaseNotes: ptr("Initial managed release"),
	}
	published, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-module-v1"}, publishBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, published.StatusCode(), string(published.Body))
	require.NotNil(t, published.JSON201)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), published.JSON201.LifecycleStatus)
	assert.Equal(t, genclient.ModuleVerificationStatus("unverified"), published.JSON201.VerificationStatus)
	assert.Empty(t, published.JSON201.ArtifactDigest)
	assert.Equal(t, ptr("1.0.0"), published.JSON201.SemanticVersion)

	deleted, err := client.DeleteModuleWithResponse(t.Context(), orgID, moduleSlug)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, deleted.StatusCode(), string(deleted.Body))
	require.NotNil(t, deleted.JSON409)
	assert.Equal(t, "module_history_retained", deleted.JSON409.Error)
	retained, err := client.GetModuleVersionWithResponse(t.Context(), orgID, moduleSlug, published.JSON201.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, retained.StatusCode(), string(retained.Body))

	replayed, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-module-v1"}, publishBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, replayed.StatusCode(), string(replayed.Body))
	assert.Equal(t, published.JSON201.Uuid, replayed.JSON201.Uuid)

	secondBody := publishBody
	secondBody.SemanticVersion = "1.1.0"
	blocked, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-module-v11"}, secondBody)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, blocked.StatusCode(), string(blocked.Body))

	promoted, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleSlug, published.JSON201.Uuid.String(),
		genclient.TransitionModuleVersionParamsLifecycleActionPromote, &genclient.TransitionModuleVersionParams{IdempotencyKey: "promote-module-v1"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: published.JSON201.ResourceVersion, Reason: "Approved first managed Default"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, promoted.StatusCode(), string(promoted.Body))
	require.NotNil(t, promoted.JSON200)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("default"), promoted.JSON200.LifecycleStatus)
	assert.Equal(t, genclient.CoreModuleVersionMigrationGeneration("v1"), promoted.JSON200.MigrationGeneration)

	legacyView, err := client.GetModuleWithResponse(t.Context(), orgID, moduleSlug)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, legacyView.StatusCode(), string(legacyView.Body))
	require.NotNil(t, legacyView.JSON200)
	assert.Equal(t, ptr("Shared payments database"), legacyView.JSON200.Description)
	legacyList, err := client.ListModulesWithResponse(t.Context(), orgID, &genclient.ListModulesParams{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, legacyList.StatusCode(), string(legacyList.Body))
	require.NotNil(t, legacyList.JSON200)
	require.Len(t, legacyList.JSON200.Items, 1)
	assert.Equal(t, legacyView.JSON200.Description, legacyList.JSON200.Items[0].Description)
	immutableVersion, err := client.GetModuleVersionWithResponse(t.Context(), orgID, moduleSlug, published.JSON201.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, immutableVersion.StatusCode(), string(immutableVersion.Body))
	require.NotNil(t, immutableVersion.JSON200)
	assert.Nil(t, immutableVersion.JSON200.Definition.Description, "catalogue metadata must not be copied into immutable definitions")

	events, err := client.ListModuleVersionLifecycleEventsWithResponse(t.Context(), orgID, moduleSlug, promoted.JSON200.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, events.StatusCode(), string(events.Body))
	require.Len(t, *events.JSON200, 2)
	assert.Nil(t, (*events.JSON200)[0].FromStatus)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), (*events.JSON200)[0].ToStatus)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("default"), (*events.JSON200)[1].ToStatus)

	prereleaseBody := publishBody
	prereleaseBody.SemanticVersion = "1.1.0-rc.1"
	prerelease, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-module-rc1"}, prereleaseBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, prerelease.StatusCode(), string(prerelease.Body))

	prereleasePromotion, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleSlug, prerelease.JSON201.Uuid.String(),
		genclient.TransitionModuleVersionParamsLifecycleActionPromote, &genclient.TransitionModuleVersionParams{IdempotencyKey: "promote-module-rc1"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: prerelease.JSON201.ResourceVersion, Reason: "Must remain Proposed"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, prereleasePromotion.StatusCode(), string(prereleasePromotion.Body))

	stableBody := publishBody
	stableBody.SemanticVersion = "1.1.0"
	graduated, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleSlug,
		prerelease.JSON201.Uuid.String(), &genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "graduate-module-rc1"},
		genclient.StableModuleVersionSuccessorBody{ExpectedPrereleaseResourceVersion: prerelease.JSON201.ResourceVersion,
			Reason: "Release candidate accepted", Version: stableBody})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, graduated.StatusCode(), string(graduated.Body))
	require.NotNil(t, graduated.JSON201)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("deprecated"), graduated.JSON201.Prerelease.LifecycleStatus)
	assert.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), graduated.JSON201.Stable.LifecycleStatus)
	assert.NotEqual(t, graduated.JSON201.Prerelease.Uuid, graduated.JSON201.Stable.Uuid)

	includeDeprecated := true
	versions, err := client.ListModuleVersionsWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.ListModuleVersionsParams{IncludeDeprecated: &includeDeprecated})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, versions.StatusCode(), string(versions.Body))
	require.NotNil(t, versions.JSON200)
	require.Len(t, versions.JSON200.Items, 3)

	detail, err := client.GetModuleVersionWithResponse(t.Context(), orgID, moduleSlug, graduated.JSON201.Stable.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, detail.StatusCode(), string(detail.Body))
	require.NotNil(t, detail.JSON200)
	assert.Equal(t, "1.1.0", detail.JSON200.Definition.VersionId)
	assert.Equal(t, graduated.JSON201.Stable.Uuid, detail.JSON200.Version.Uuid)

	comparison, err := client.CompareModuleVersionsWithResponse(t.Context(), orgID, moduleSlug,
		published.JSON201.Uuid.String(), graduated.JSON201.Stable.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, comparison.StatusCode(), string(comparison.Body))
	require.NotNil(t, comparison.JSON200)
	assert.Equal(t, published.JSON201.Uuid, comparison.JSON200.FromVersionUuid)
	assert.Equal(t, graduated.JSON201.Stable.Uuid, comparison.JSON200.ToVersionUuid)
	assert.Equal(t, published.JSON201.ArtifactDigest, comparison.JSON200.Before.ArtifactDigest)
	assert.Equal(t, graduated.JSON201.Stable.ArtifactDigest, comparison.JSON200.After.ArtifactDigest)
	assert.Equal(t, published.JSON201.SourceRevision, comparison.JSON200.Before.SourceRevision)
	assert.Equal(t, graduated.JSON201.Stable.SourceRevision, comparison.JSON200.After.SourceRevision)
	assert.False(t, comparison.JSON200.ResourceTypeChanged)
}

func TestEnvironmentModuleVersionPinPersistenceAndOperationLock(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	resourceType := MustCreateResourceType(t, client, orgID, "pin-"+strings.ToLower(rand.Text()))
	moduleSlug := "pin-release-" + strings.ToLower(rand.Text())
	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: "create-pin-module"},
		genclient.ModuleCatalogueCreateBody{Slug: moduleSlug, ResourceType: resourceType.Id})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))

	body := genclient.ModuleVersionPublishBody{SemanticVersion: "1.0.0", ModuleSource: "inline",
		ModuleSourceCode: ptr("output \"name\" { value = \"pin-test\" }"), ModuleInputs: map[string]any{},
		ModuleParams: map[string]genclient.ModuleParamItem{}, ProviderMapping: map[string]string{},
		Dependencies: map[string]genclient.ModuleDependencyManifest{}, Coprovisioned: []genclient.ModuleCoProvisionManifest{}}
	v1, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-pin-v1"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, v1.StatusCode(), string(v1.Body))
	promoted, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleSlug, v1.JSON201.Uuid.String(),
		genclient.TransitionModuleVersionParamsLifecycleActionPromote,
		&genclient.TransitionModuleVersionParams{IdempotencyKey: "promote-pin-v1"},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: v1.JSON201.ResourceVersion, Reason: "Establish pinned Default"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, promoted.StatusCode(), string(promoted.Body))
	body.SemanticVersion = "2.0.0"
	v2, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleSlug,
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-pin-v2"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, v2.StatusCode(), string(v2.Body))

	database := MustDatabaser(t)
	actor, projectUUID, environmentUUID := uuid.New(), uuid.New(), uuid.New()
	tx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	pin, err := database.CreateEnvironmentModuleVersionPin(t.Context(), tx, orgID, projectUUID, "video-project",
		environmentUUID, "video-staging", created.JSON201.Uuid, promoted.JSON200.Uuid, actor, "user",
		"Freeze the demonstrated deployment", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	activationBoundary := pin.ActivationEventID
	pinResourceVersion := pin.ResourceVersion

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	note, err := database.AppendModuleVersionPinNote(t.Context(), tx, orgID, pin.ID, actor, "user", "Keep this Pin through the conference demo")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, "note_added", note.EventType)
	require.Equal(t, activationBoundary, note.ActivationEventID)
	require.Equal(t, int64(2), note.Revision)

	afterNote, err := database.GetEnvironmentModuleVersionPin(t.Context(), nil, orgID, pin.ID, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, pinResourceVersion, afterNote.ResourceVersion, "a note is not an optimistic-concurrency state change")
	require.Equal(t, activationBoundary, afterNote.ActivationEventID, "a note must not invalidate Pin-aware approvals")

	operationID := uuid.New()
	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	pending, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pin.ResourceVersion,
		moduleversions.PinOverridePending, "override_requested", actor, "addon", "Roll out v2", &operationID, &v2.JSON201.Uuid, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, activationBoundary, pending.ActivationEventID, "pending override must preserve the approval boundary")

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	otherOperation := uuid.New()
	_, err = database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pending.ResourceVersion,
		moduleversions.PinOverridden, "override_succeeded", actor, "addon", "Wrong operation", &otherOperation, &v2.JSON201.Uuid, ptr(uuid.New()))
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	failed, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pending.ResourceVersion,
		moduleversions.PinActive, "override_failed", actor, "addon", "Deployment failed before adoption", &operationID, &v2.JSON201.Uuid, ptr(uuid.New()))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, activationBoundary, failed.ActivationEventID)

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	pending, err = database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, failed.ResourceVersion,
		moduleversions.PinOverridePending, "override_requested", actor, "addon", "Retry after manual recovery", &operationID, &v2.JSON201.Uuid, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	cancelled, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pending.ResourceVersion,
		moduleversions.PinActive, "override_cancelled", actor, "addon", "Operator cancelled before adoption", &operationID, &v2.JSON201.Uuid, ptr(uuid.New()))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, activationBoundary, cancelled.ActivationEventID)

	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	pending, err = database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, cancelled.ResourceVersion,
		moduleversions.PinOverridePending, "override_requested", actor, "addon", "Retry after cancellation", &operationID, &v2.JSON201.Uuid, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	deploymentID := uuid.New()
	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	overridden, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, pending.ResourceVersion,
		moduleversions.PinOverridden, "override_succeeded", actor, "addon", "Target deployment succeeded", &operationID, &v2.JSON201.Uuid, &deploymentID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, activationBoundary, overridden.ActivationEventID)

	rollbackDeploymentID := uuid.New()
	tx, err = database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	restored, err := database.TransitionEnvironmentModuleVersionPin(t.Context(), tx, orgID, pin.ID, overridden.ResourceVersion,
		moduleversions.PinActive, "restored_after_rollback", actor, "addon", "Rollback restored exact pinned version",
		&operationID, nil, &rollbackDeploymentID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NotEqual(t, activationBoundary, restored.ActivationEventID, "restoration establishes a fresh approval boundary")

	events, err := database.ListModuleVersionPinEvents(t.Context(), nil, orgID, pin.ID)
	require.NoError(t, err)
	require.Len(t, events, 9)
	require.Equal(t, int64(9), events[len(events)-1].Revision)
	require.Equal(t, "note_added", events[1].EventType)
}

func ptr[T any](value T) *T { return &value }
