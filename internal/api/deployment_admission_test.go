package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func TestDeploymentAdmissionRejectsPluginServiceMisconfiguration(t *testing.T) {
	service := userid.NewServiceUserTokenId()
	item := plugins.Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service.String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(), Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.admit", Permission: "plugin.dev.example.workflow.control", Scope: "environment"}}}
	registry, err := plugins.NewInstallationRegistry([]plugins.Installation{item}, false)
	require.NoError(t, err)
	s := &Server{DeploymentAdmissionIdentity: service, PluginGateway: &plugins.Gateway{Registry: registry}}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, service.String())
	require.ErrorContains(t, s.deploymentAdmissionCaller(ctx, "acme"), "plugin service cannot act")
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
