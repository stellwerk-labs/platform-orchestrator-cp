package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/hmessaging"
	mockorchestratordp "github.com/stellwerk-labs/platform-orchestrator-cp/internal/clients/orchestratordp/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	mockmodel "github.com/stellwerk-labs/platform-orchestrator-cp/internal/model/mocks"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

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

func TestPinOverrideReconciliationRequiresCurrentOwnedPendingState(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	database := mockmodel.NewMockDatabaser(ctrl)
	tx := mockmodel.NewMockTxWithCommit(ctrl)
	database.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)

	pinID, operationID, deploymentID := uuid.New(), uuid.New(), uuid.New()
	pin := model.EnvironmentModuleVersionPin{
		ID: pinID, OrgID: "acme", ProjectUUID: uuid.New(), ProjectID: "retail",
		EnvironmentUUID: uuid.New(), EnvironmentID: "production", ModuleUUID: uuid.New(),
		VersionUUID: uuid.New(), Status: moduleversions.PinActive, ResourceVersion: 7,
		ActivationEventID: uuid.New(), CreatedBy: userid.InternalSystemUuid,
	}

	database.EXPECT().GetEnvironmentModuleVersionPin(gomock.Any(), nil, "acme", pinID, model.GetModeDefault).Return(&pin, nil)
	database.EXPECT().GetModuleCoreCommand(gomock.Any(), tx, "acme",
		"pin-reconcile:"+pinID.String()+":"+operationID.String()+":"+deploymentID.String(), "stale-callback",
	).Return("", nil, false, nil)
	database.EXPECT().GetEnvironmentModuleVersionPin(gomock.Any(), tx, "acme", pinID, model.GetModeForUpdate).Return(&pin, nil)

	server := &Server{
		Database: database, Logger: zaptest.NewLogger(t), Publisher: new(hmessaging.RecordingPublisher),
		DpClient:  mockorchestratordp.NewMockClientWithResponsesInterface(ctrl),
		IamClient: mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl),
	}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())

	result, err := server.ReconcileEnvironmentModuleVersionPinOverride(ctx, ReconcileEnvironmentModuleVersionPinOverrideRequestObject{
		OrgId: "acme", PinId: pinID, Params: ReconcileEnvironmentModuleVersionPinOverrideParams{IdempotencyKey: "stale-callback"},
		Body: &ModuleVersionPinOverrideReconcileBody{
			ExpectedResourceVersion: pin.ResourceVersion, OperationId: operationID, DeploymentId: deploymentID,
			Outcome: ModuleVersionPinOverrideReconcileBodyOutcomeFailed, Reason: "Authoritative failure from old callback",
		},
	})

	require.NoError(t, err)
	response := result.(ReconcileEnvironmentModuleVersionPinOverride409JSONResponse)
	assert.Equal(t, "Pin is not override-pending for this operation", response.Message)
}

func TestPinRollbackRestorationRequiresCurrentOwnedOverriddenState(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	database := mockmodel.NewMockDatabaser(ctrl)
	tx := mockmodel.NewMockTxWithCommit(ctrl)
	database.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)

	pinID, operationID, deploymentID := uuid.New(), uuid.New(), uuid.New()
	pin := model.EnvironmentModuleVersionPin{
		ID: pinID, OrgID: "acme", ProjectUUID: uuid.New(), ProjectID: "retail",
		EnvironmentUUID: uuid.New(), EnvironmentID: "production", ModuleUUID: uuid.New(),
		VersionUUID: uuid.New(), Status: moduleversions.PinActive, ResourceVersion: 11,
		ActivationEventID: uuid.New(), CreatedBy: userid.InternalSystemUuid,
	}

	database.EXPECT().GetEnvironmentModuleVersionPin(gomock.Any(), nil, "acme", pinID, model.GetModeDefault).Return(&pin, nil)
	database.EXPECT().GetModuleCoreCommand(gomock.Any(), tx, "acme",
		"pin-restore:"+pinID.String()+":"+operationID.String()+":"+deploymentID.String(), "stale-rollback",
	).Return("", nil, false, nil)
	database.EXPECT().GetEnvironmentModuleVersionPin(gomock.Any(), tx, "acme", pinID, model.GetModeForUpdate).Return(&pin, nil)

	server := &Server{
		Database: database, Logger: zaptest.NewLogger(t), Publisher: new(hmessaging.RecordingPublisher),
		DpClient:  mockorchestratordp.NewMockClientWithResponsesInterface(ctrl),
		IamClient: mockorchestratoriam.NewMockClientWithResponsesInterface(ctrl),
	}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, userid.InternalSystemUuid.String())

	result, err := server.RestoreEnvironmentModuleVersionPinAfterRollback(ctx, RestoreEnvironmentModuleVersionPinAfterRollbackRequestObject{
		OrgId: "acme", PinId: pinID, Params: RestoreEnvironmentModuleVersionPinAfterRollbackParams{IdempotencyKey: "stale-rollback"},
		Body: &ModuleVersionPinRollbackRestoreBody{
			ExpectedResourceVersion: pin.ResourceVersion, OperationId: operationID, DeploymentId: deploymentID,
			RestoredVersionUuid: pin.VersionUUID, Reason: "Rollback callback after protection was already active",
		},
	})

	require.NoError(t, err)
	response := result.(RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse)
	assert.Equal(t, "Pin was not overridden by this operation", response.Message)
}
