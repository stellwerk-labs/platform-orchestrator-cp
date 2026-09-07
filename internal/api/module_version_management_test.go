package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	mockorchestratoriam "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratoriam/mocks"
)

func TestStructuralKeyDiffReturnsContractSafeEmptyArrays(t *testing.T) {
	added, removed, changed := structuralKeyDiff(map[string]any{}, map[string]any{})

	require.NotNil(t, added)
	require.NotNil(t, removed)
	require.NotNil(t, changed)
	assert.Empty(t, added)
	assert.Empty(t, removed)
	assert.Empty(t, changed)
}

func TestStructuralKeyDiffClassifiesAndSortsChanges(t *testing.T) {
	left := map[string]any{"removed-z": true, "changed": "before", "same": 1}
	right := map[string]any{"added-z": true, "added-a": true, "changed": "after", "same": 1}

	added, removed, changed := structuralKeyDiff(left, right)

	assert.Equal(t, []string{"added-a", "added-z"}, added)
	assert.Equal(t, []string{"removed-z"}, removed)
	assert.Equal(t, []string{"changed"}, changed)
}

func TestModuleVersionComparisonSnapshotExposesTypedDefinitionValues(t *testing.T) {
	snapshot := moduleVersionComparisonSnapshot(model.ModuleDefinitionVersion{
		ModuleSource:    "inline",
		ArtifactDigest:  "",
		SourceRevision:  "revision-one",
		ResourceType:    "workload",
		ModuleInputs:    map[string]any{"replicas": 3.0},
		ProviderMapping: map[string]string{"kubernetes": "demo"},
		Dependencies:    map[string]model.ModuleDefinitionDependency{},
		CoProvisioned:   []model.ModuleDefinitionCoProvision{},
	})

	assert.Equal(t, "inline", snapshot.ModuleSource)
	assert.Equal(t, "revision-one", snapshot.SourceRevision)
	assert.Equal(t, "workload", snapshot.ResourceType)
	assert.Equal(t, map[string]any{"replicas": 3.0}, snapshot.ModuleInputs)
	assert.Equal(t, map[string]string{"kubernetes": "demo"}, snapshot.ProviderMapping)
	require.NotNil(t, snapshot.Dependencies)
	require.NotNil(t, snapshot.Coprovisioned)
}

func TestUnpinIsAuthorizedByScopeRatherThanPinCreator(t *testing.T) {
	_, server, cleanup := MockServer(t)
	defer cleanup()

	database := server.Database.(*mockmodel.MockDatabaser)
	iamClient := server.IamClient.(*mockorchestratoriam.MockClientWithResponsesInterface)
	creator, actor := userid.NewHumanUserId(), userid.NewHumanUserId()
	projectUUID, environmentUUID, pinID := uuid.New(), uuid.New(), uuid.New()
	pin := model.EnvironmentModuleVersionPin{
		ID: pinID, OrgID: "acme", ProjectUUID: projectUUID, ProjectID: "retail",
		EnvironmentUUID: environmentUUID, EnvironmentID: "production", ModuleUUID: uuid.New(),
		VersionUUID: uuid.New(), Status: moduleversions.PinActive, ResourceVersion: 4,
		ActivationEventID: uuid.New(), CreatedBy: creator,
	}
	environment := model.Environment{Uuid: environmentUUID, ProjectId: "retail", Id: "production"}

	database.EXPECT().GetEnvironmentModuleVersionPin(gomock.Any(), nil, "acme", pinID, model.GetModeDefault).Return(&pin, nil)
	database.EXPECT().GetEnvironmentByUuid(gomock.Any(), nil, "acme", environmentUUID, model.GetModeDefault).Return(&environment, nil)
	database.EXPECT().GetEnvironment(gomock.Any(), nil, "acme", "retail", "production", model.GetModeDefault).Return(&environment, nil)
	iamClient.EXPECT().InternalAuthorizeWithResponse(gomock.Any(), orchestratoriam.InternalAuthorizeBody{
		UserId: actor,
		Checks: []orchestratoriam.ResourcePermissionCheck{environmentCheck(environmentUUID, PermissionModuleVersionUnpin)},
	}).Return(&orchestratoriam.InternalAuthorizeResponse{HTTPResponse: &http.Response{StatusCode: http.StatusNoContent}}, nil)
	database.EXPECT().GetModuleCoreCommand(gomock.Any(), gomock.Not(nil), "acme", "pin:"+pinID.String()+":unpin", "unpin-video").Return("", nil, false, nil)
	removed := pin
	removed.Status = moduleversions.PinRemoved
	removed.ResourceVersion++
	database.EXPECT().TransitionEnvironmentModuleVersionPin(
		gomock.Any(), gomock.Not(nil), "acme", pinID, int64(4), moduleversions.PinRemoved,
		"unpin", actor, "user", "Release the environment", nil, nil, nil,
	).Return(&removed, nil)
	database.EXPECT().StoreModuleCoreCommand(
		gomock.Any(), gomock.Not(nil), "acme", "pin:"+pinID.String()+":unpin", "unpin-video",
		gomock.Any(), actor, gomock.Any(),
	).Return(nil)

	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, actor.String())
	result, err := server.TransitionEnvironmentModuleVersionPin(ctx, TransitionEnvironmentModuleVersionPinRequestObject{
		OrgId: "acme", PinId: pinID, PinAction: TransitionEnvironmentModuleVersionPinParamsPinActionUnpin,
		Params: TransitionEnvironmentModuleVersionPinParams{IdempotencyKey: "unpin-video"},
		Body:   &ModuleReasonedCommand{ExpectedResourceVersion: 4, Reason: "Release the environment"},
	})

	require.NoError(t, err)
	response := result.(TransitionEnvironmentModuleVersionPin200JSONResponse)
	assert.Equal(t, ModuleVersionPinStatus(moduleversions.PinRemoved), response.Status)
	assert.NotEqual(t, creator, actor, "the test must exercise a different scope-authorized actor")
}
