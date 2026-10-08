//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"slices"
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
	releaseOperationGrants(t, db, grant.PluginID)

	uninstalled := steps.installation(s.PluginManager.Transition(ctx, uninstall, id, pluginmanager.ActionUninstall))
	require.Equal(t, model.PluginInstallationUninstalled, uninstalled.State)
	require.Empty(t, uninstalled.Organizations)
	requireEffectCode(t, effect(true), "plugin_not_installed")
	_, _, found = s.PluginGateway.Registry.Installation(ctx, grant.OrgID, grant.PluginID)
	require.False(t, found, "an uninstalled plugin is not served")

	reinstalled := steps.installation(s.PluginManager.Transition(ctx, steps.command("Reinstall the plugin"), id, pluginmanager.ActionReinstall))
	require.Equal(t, id, reinstalled.InstallationID, "reinstall reuses the UUID the licence binds to")
	steps.installation(s.PluginManager.Transition(ctx, steps.command("Serve it again"), id, pluginmanager.ActionActivate))
	steps.installation(s.PluginManager.PutEnablement(ctx, steps.command("Enable the organization again"), id, grant.OrgID, model.PluginEnablementEnabled, "", nil, item.ServiceUserID))
	again, descriptor, found := s.PluginGateway.Registry.Installation(ctx, grant.OrgID, grant.PluginID)
	require.True(t, found)
	require.Equal(t, plugins.StateAvailable, descriptor.State)
	require.Equal(t, item.InstallationID, again.InstallationID)
}

func TestOperatorAPIUpgradesAndReinstallsWithoutLosingTheLicence(t *testing.T) {
	f := newPluginEntitlementAPIFixture(t)
	ctx := t.Context()
	registry := &plugins.InstallationRegistry{Entitlements: f.adapter}
	manager := testPluginManager(t, f.db, registry, f.adapter)
	require.NoError(t, manager.Refresh(ctx))
	f.server.PluginManager, f.server.PluginEntitlements, f.server.PluginGateway = manager, f.adapter, &plugins.Gateway{Registry: registry}
	api := newOperatorAPI(t, f.server)
	api.iam.createServiceUser("acme", uuid.MustParse(f.item.ServiceUserID))
	id := uuid.MustParse(f.item.InstallationID)
	path := "/plugin-installations/" + id.String()
	licensed := func(why string) plugins.Installation {
		t.Helper()
		item, descriptor, found := registry.Installation(ctx, "acme", f.item.ID)
		require.True(t, found, why)
		require.Equal(t, plugins.StateAvailable, descriptor.State, why)
		require.Equal(t, "valid", descriptor.Entitlement.Features["feature-a"].State, why)
		require.Equal(t, id.String(), item.InstallationID, why)
		tx, err := f.server.pluginEffectDatabase("acme", item, "effect.commit", false).BeginTx(ctx, nil)
		require.NoError(t, err, why)
		require.NoError(t, tx.Rollback())
		return item
	}
	step := func(method, route string, body any) PluginInstallation {
		t.Helper()
		return decode[PluginInstallation](t, api.call(pluginOperator, method, route, uuid.NewString(), body), http.StatusOK)
	}
	licensed("the fixture is licensed")

	successor := f.item
	successor.Version = "1.1.0"
	successor.Operations = append(slices.Clone(f.item.Operations), plugins.Operation{ID: "status", Method: "GET", Path: "/orgs/{orgId}/status", RequiredFeature: "feature-a"})
	manifest, err := json.Marshal(successor.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	decode[PluginPackage](t, api.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: api.revision(), Reason: "Register the successor", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)
	upgrading := step(http.MethodPost, path+"/upgrade", PluginInstallationUpgrade{ExpectedRevision: api.revision(), Reason: "Upgrade the plugin", PackageSha256: digest})
	require.Equal(t, f.item.ManifestSHA256, upgrading.PackageSha256)
	require.Equal(t, digest, *upgrading.TargetPackageSha256)
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/activate", uuid.NewString(), command(api.revision(), "Switch too early")),
		http.StatusConflict, "plugin_package_digest_mismatch")
	pending := decode[PluginInstallation](t, api.call(pluginOperator, http.MethodGet, path, "", nil), http.StatusOK)
	require.Equal(t, PluginAvailabilityStateAvailable, pending.Availability, "the predecessor stays served")
	require.Equal(t, PluginDiagnosticPackageDigestMismatch, pending.Diagnostic.Code)
	require.Equal(t, f.item.ManifestSHA256, *pending.Observation.PackageSha256, "Core shows what the plugin reported")
	require.Len(t, licensed("the predecessor stays licensed during the rolling update").Operations, 1)

	fleet.serve(t, f.item.Backend, id, digest)
	switched := step(http.MethodPost, path+"/actions/activate", command(api.revision(), "Switch to the successor"))
	require.Equal(t, digest, switched.PackageSha256)
	require.Nil(t, switched.TargetPackageSha256)
	require.Nil(t, switched.Diagnostic)
	served := licensed("an upgrade keeps the licence of the installation UUID")
	require.Equal(t, digest, served.ManifestSHA256)
	require.Len(t, served.Operations, 2)

	requireErrorCode(t, api.call(pluginOperator, http.MethodDelete, path, uuid.NewString(), command(api.revision(), "Purge too early")),
		http.StatusConflict, "plugin_installation_state")
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/uninstall", uuid.NewString(), command(api.revision(), "Uninstall too early")),
		http.StatusConflict, "plugin_work_unsettled")
	releaseOperationGrants(t, f.db, f.item.ID)
	uninstalled := step(http.MethodPost, path+"/actions/uninstall", command(api.revision(), "Uninstall the plugin"))
	require.Equal(t, PluginInstallationStateUninstalled, uninstalled.State)
	require.Equal(t, PluginAvailabilityStateNotInstalled, uninstalled.Availability)
	reinstalled := step(http.MethodPost, path+"/actions/reinstall", command(api.revision(), "Reinstall the plugin"))
	require.Equal(t, id, reinstalled.InstallationId)
	require.Equal(t, PluginInstallationStateInstalled, reinstalled.State)
	step(http.MethodPost, path+"/actions/activate", command(api.revision(), "Serve it again"))
	timezone := "UTC"
	step(http.MethodPut, path+"/organizations/acme", PluginEnablementPut{ExpectedRevision: api.revision(), Reason: "Enable acme again", State: PluginEnablementPutStateEnabled, Timezone: &timezone})
	licensed("a reinstall reuses the UUID, so the imported licence matches again")

	step(http.MethodPost, path+"/actions/uninstall", command(api.revision(), "Uninstall for good"))
	purged := step(http.MethodDelete, path, command(api.revision(), "Retire the installation"))
	require.Equal(t, PluginInstallationStatePurged, purged.State)
	requireErrorCode(t, api.call(pluginOperator, http.MethodPost, path+"/actions/reinstall", uuid.NewString(), command(api.revision(), "Too late")),
		http.StatusConflict, "plugin_installation_state")
	notAnAction := api.call(pluginOperator, http.MethodPost, path+"/actions/purge", uuid.NewString(), command(api.revision(), "Not an action"))
	require.Equal(t, http.StatusBadRequest, notAnAction.Code, "purge is the explicit DELETE, never an action: %s", notAnAction.Body.String())
}

// releaseOperationGrants releases every operation grant of the plugin, as the
// plugin does when its operations finish.
func releaseOperationGrants(t *testing.T, db model.Databaser, plugin string) {
	t.Helper()
	ctx := t.Context()
	type held struct {
		org       string
		operation uuid.UUID
	}
	unreleased := func() []held {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, `SELECT org_id, operation_id FROM external_operation_grants WHERE plugin_id=$1 AND state<>'released'`, plugin)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var grants []held
		for rows.Next() {
			var grant held
			require.NoError(t, rows.Scan(&grant.org, &grant.operation))
			grants = append(grants, grant)
		}
		require.NoError(t, rows.Err())
		return grants
	}
	for _, held := range unreleased() {
		grant, err := model.GetExternalOperationGrant(ctx, db, held.org, plugin, held.operation)
		require.NoError(t, err)
		_, err = model.ApplyExternalGrantCommand(ctx, db, model.ExternalGrantCommand{OrgID: held.org, PluginID: plugin, OperationID: held.operation, ActorID: grant.ActorID,
			SourceOperation: "controlWorkflow", SourceDigest: strings.Repeat("a", 64), RequestID: "finish", Action: "release", Reason: "Finish the operation", ExpectedRevision: grant.Revision})
		require.NoError(t, err)
	}
}
