package api

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func TestDeploymentAdmissionRejectsPluginServiceMisconfiguration(t *testing.T) {
	service := userid.NewServiceUserTokenId()
	item := plugins.Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", ServiceUserID: service.String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(), Organizations: map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, ServiceGrants: []plugins.ServiceGrant{{Action: "external-deployment.admit", Permission: "plugin.dev.example.workflow.control", Scope: "environment"}}}
	encoded, err := json.Marshal([]plugins.Installation{item})
	require.NoError(t, err)
	registry, err := plugins.LoadInstallations(bytes.NewReader(encoded), false)
	require.NoError(t, err)
	s := &Server{DeploymentAdmissionIdentities: map[string]uuid.UUID{"acme": service}, PluginGateway: &plugins.Gateway{Registry: registry}}
	ctx := context.WithValue(t.Context(), hecho.ContextKeyUserID, service.String())
	require.ErrorContains(t, s.deploymentAdmissionCaller(ctx, "acme"), "plugin service cannot act")
}
