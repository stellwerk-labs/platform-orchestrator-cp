//go:build integration

package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

func requirePluginManagerConflict(t *testing.T, err error, code string) {
	t.Helper()
	conflict, ok := model.IsErrConflict(err)
	require.True(t, ok, "expected %s, got %v", code, err)
	require.Equal(t, code, conflict.Code)
}

func TestUninstallWaitsUntilCoreHoldsNoSafetyWork(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	s := pluginManagerServer(t, db)
	ctx := t.Context()
	id := newOperatorAPI(t, s).install(workflowPlugin(grant, "external-deployment.admit"), grant.OrgID).InstallationId
	item, _, found := s.PluginGateway.Registry.Installation(ctx, grant.OrgID, grant.PluginID)
	require.True(t, found)
	effect := func(safety bool) error {
		tx, err := s.pluginEffectDatabase(grant.OrgID, item, "external-deployment.admit", safety).BeginTx(ctx, nil)
		if err == nil {
			require.NoError(t, tx.Rollback())
		}
		return err
	}
	environment := grant.Intent.Environments[0]
	dispatch := model.ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision,
		Action: "external-deployment.admit", DispatchID: "environment-1", ProjectUUID: environment.ProjectID, EnvironmentUUID: environment.EnvironmentID,
		RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	require.NoError(t, model.RecordExternalDispatch(ctx, db, dispatch))

	steps := newOperatorSteps(t, s.PluginManager)
	steps.installation(s.PluginManager.Transition(ctx, steps.command("Stop new work before the uninstall"), id, pluginmanager.ActionDisable))
	// The operator decides once and retries the same command until Core
	// holds no safety work; every refusal changes nothing.
	uninstall := steps.command("Uninstall the plugin")
	refused := func(why string) {
		t.Helper()
		_, err := s.PluginManager.Transition(ctx, uninstall, id, pluginmanager.ActionUninstall)
		requirePluginManagerConflict(t, err, "plugin_work_unsettled")
		installation, err := s.PluginManager.Installation(ctx, id)
		require.NoError(t, err)
		require.Equal(t, model.PluginInstallationDisabled, installation.State, why)
		require.Len(t, installation.Organizations, 1, why)
		require.NoError(t, effect(true), "%s: cancel, rollback and read stay available", why)
		requireEffectCode(t, effect(false), "plugin_not_enabled")
	}
	refused("an active grant and an unsettled dispatch")

	require.NoError(t, model.CompleteExternalDispatch(ctx, db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, model.ExternalDispatchCompletion{
		DeploymentID: uuid.New(), Status: "succeeded", EnvironmentUUID: environment.EnvironmentID, ProjectUUID: environment.ProjectID, RequestDigest: dispatch.RequestDigest}))
	refused("the grant is still active")
	_, err := model.ApplyExternalGrantCommand(ctx, db, model.ExternalGrantCommand{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID,
		ActorID: grant.ActorID, SourceOperation: "controlWorkflow", SourceDigest: strings.Repeat("a", 64), RequestID: "finish", Action: "release",
		Reason: "Finish the operation", ExpectedRevision: grant.Revision})
	require.NoError(t, err)

	uninstalled := steps.installation(s.PluginManager.Transition(ctx, uninstall, id, pluginmanager.ActionUninstall))
	require.Equal(t, model.PluginInstallationUninstalled, uninstalled.State)
	require.Empty(t, uninstalled.Organizations)
	requireEffectCode(t, effect(true), "plugin_not_installed")
	_, _, found = s.PluginGateway.Registry.Installation(ctx, grant.OrgID, grant.PluginID)
	require.False(t, found, "an uninstalled plugin is not served")

	reinstalled := steps.installation(s.PluginManager.Transition(ctx, steps.command("Reinstall the plugin"), id, pluginmanager.ActionReinstall))
	require.Equal(t, id, reinstalled.InstallationID, "reinstall reuses the UUID the licence binds to")
	steps.installation(s.PluginManager.Transition(ctx, steps.command("Serve it again"), id, pluginmanager.ActionActivate))
	steps.installation(s.PluginManager.PutEnablement(ctx, steps.command("Enable the organization again"), id, grant.OrgID, model.PluginEnablementEnabled, "", nil))
	again, descriptor, found := s.PluginGateway.Registry.Installation(ctx, grant.OrgID, grant.PluginID)
	require.True(t, found)
	require.Equal(t, plugins.StateAvailable, descriptor.State)
	require.Equal(t, item.InstallationID, again.InstallationID)
}
