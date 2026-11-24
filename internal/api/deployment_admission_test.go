package api

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"testing"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

func TestDeploymentAdmissionFailsClosedWithoutIdentityDatabase(t *testing.T) {
	service := userid.NewServiceUserTokenId()
	s := &Server{DeploymentAdmissionIdentity: service}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, service.String())
	var boundary *herrors.PlatformOrchestratorError
	require.ErrorAs(t, s.deploymentAdmissionCaller(ctx, "acme"), &boundary)
	require.Equal(t, http.StatusServiceUnavailable, boundary.StatusCode)
	require.Equal(t, "authorization_unavailable", boundary.Code)
}

func TestDeploymentAdmissionConflictsKeepTheirStableCode(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"busy Environment", model.ErrConflict{Code: model.EnvironmentBusy, Message: "Environment has an unresolved ordinary Deployment admission"}, model.EnvironmentBusy},
		{"Pin protection", model.ErrConflict{Code: model.PinProtection, Message: "resolved Deployment does not preserve current Pin protection"}, model.PinProtection},
		{"uncoded conflict", model.NewErrConflict("admission identity was settled or belongs to a different exact request"), "deployment_admission_conflict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var boundary *herrors.PlatformOrchestratorError
			require.ErrorAs(t, deploymentAdmissionModelError(test.err), &boundary)
			require.Equal(t, http.StatusConflict, boundary.StatusCode)
			require.Equal(t, test.want, boundary.Code)
		})
	}
}

// A plugin learns that its dispatch must wait, but nothing else about other operations.
func TestPluginDispatchConflictsExposeOnlyEnvironmentBusy(t *testing.T) {
	var boundary *herrors.PlatformOrchestratorError
	require.ErrorAs(t, pluginModelError(model.ErrConflict{Code: model.EnvironmentBusy, Message: "Environment has an unresolved external Deployment dispatch"}), &boundary)
	require.Equal(t, http.StatusConflict, boundary.StatusCode)
	require.Equal(t, model.EnvironmentBusy, boundary.Code)
	require.ErrorAs(t, pluginModelError(model.ErrConflict{Code: model.PinProtection, Message: "secret detail"}), &boundary)
	require.Equal(t, "plugin_operation_conflict", boundary.Code)
	require.NotContains(t, boundary.Message, "secret detail")
}

func TestDraftTestAdmissionConversionPreservesExplicitSelection(t *testing.T) {
	module, snapshot := uuid.NewString(), uuid.New()
	selection := map[string]uuid.UUID{module: snapshot}
	body := DeploymentAdmissionRequest{ModuleVersions: selection, DraftTestSnapshots: &selection}
	converted := deploymentAdmissionFromAPI("example", uuid.New(), &body)
	require.Equal(t, selection, converted.DraftTestSnapshots)
	body.DraftTestSnapshots = nil
	converted = deploymentAdmissionFromAPI("example", uuid.New(), &body)
	require.Nil(t, converted.DraftTestSnapshots, "ordinary requests cannot acquire implicit Draft test provenance")
}

func TestNativeTargetAdmissionConversionPreservesExactReference(t *testing.T) {
	target := &NativeExecutionTargetReference{RunnerId: "pool", RunnerUuid: uuid.New(), RuntimeVersion: NativeTargetRuntimeV2, PipelineVersion: NativeTargetPipelineV2, ToolsDigest: strings.Repeat("a", 64), StateBackendType: StateStorageTypeS3, StateConfigurationDigest: strings.Repeat("b", 64)}
	body := DeploymentAdmissionRequest{NativeTarget: target}
	converted := deploymentAdmissionFromAPI("example", uuid.New(), &body)
	expected, err := json.Marshal(target)
	require.NoError(t, err)
	actual, err := json.Marshal(converted.NativeTarget)
	require.NoError(t, err)
	require.JSONEq(t, string(expected), string(actual))
	body.NativeTarget = nil
	converted = deploymentAdmissionFromAPI("example", uuid.New(), &body)
	require.Nil(t, converted.NativeTarget)
	encoded, err := json.Marshal(converted)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "native_target", "historical intent bytes must retain absence")
}

func TestNativeTargetIdentifierUsesRunnerGrammarAndRuntimeLimit(t *testing.T) {
	spec, err := GetSwagger()
	require.NoError(t, err)
	created := spec.Components.Schemas["RunnerCreateBody"].Value.Properties["id"].Value
	target := spec.Components.Schemas["NativeExecutionTargetReference"].Value.Properties["runner_id"].Value
	require.Equal(t, created.Pattern, target.Pattern, "Runner and Resource/Module identifiers are different contracts")
	for _, id := range []string{"np", "native_pool", "n-", "list", "new", "by-id", strings.Repeat("n", 128)} {
		require.NoError(t, target.VisitJSON(id))
	}
	for _, id := range []string{"n", "native.pool", "Native", strings.Repeat("n", 129)} {
		require.Error(t, target.VisitJSON(id))
	}
}
