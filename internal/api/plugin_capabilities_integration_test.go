//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	iamapi "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// held returns the capabilities IAM holds for plugin in org.
func (p *iamPeer) held(org, plugin string) []iamapi.PluginCapabilityRegistration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.catalogue[org][plugin])
}

// replace changes IAM's catalogue of plugin in org behind Core's back, as an
// organization administrator holding plugin_registration_write can.
func (p *iamPeer) replace(org, plugin string, capabilities []iamapi.PluginCapabilityRegistration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.catalogue[org] == nil {
		p.catalogue[org] = map[string][]iamapi.PluginCapabilityRegistration{}
	}
	p.catalogue[org][plugin] = capabilities
}

// replaced returns how often Core replaced each organization's catalogue.
func (p *iamPeer) replaced() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	counts := map[string]int{}
	for org, count := range p.replacements {
		counts[org] = count
	}
	return counts
}

// registrations are the IAM registrations of the package capabilities.
func registrations(plugin string, capabilities []plugins.Capability) []iamapi.PluginCapabilityRegistration {
	result := []iamapi.PluginCapabilityRegistration{}
	for _, capability := range capabilities {
		scopes := []iamapi.PluginCapabilityRegistrationScopes{}
		for _, scope := range capability.Scopes {
			scopes = append(scopes, iamapi.PluginCapabilityRegistrationScopes(scope))
		}
		result = append(result, iamapi.PluginCapabilityRegistration{Name: strings.TrimPrefix(capability.ID, "plugin."+plugin+"."), DisplayName: capability.DisplayName,
			Description: capability.Description, Category: capability.Category, Level: iamapi.PluginCapabilityRegistrationLevel(capability.Level), Scopes: scopes})
	}
	return result
}

// capabilityStates returns each enabled organization's capability
// registration as the operator reads the installation.
func (a *operatorAPI) capabilityStates(installation uuid.UUID) map[string]PluginCapabilityRegistration {
	a.t.Helper()
	read := decode[PluginInstallation](a.t, a.call(pluginOperator, http.MethodGet, "/plugin-installations/"+installation.String(), "", nil), http.StatusOK)
	states := map[string]PluginCapabilityRegistration{}
	for _, enablement := range read.Organizations {
		states[enablement.OrgId] = enablement.CapabilityRegistration
	}
	return states
}

// put changes org's enablement of installation.
func (a *operatorAPI) put(installation uuid.UUID, org string, state PluginEnablementPutState) PluginInstallation {
	a.t.Helper()
	return decode[PluginInstallation](a.t, a.call(pluginOperator, http.MethodPut, "/plugin-installations/"+installation.String()+"/organizations/"+org, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: a.revision(), Reason: "Enable " + org, State: state}), http.StatusOK)
}

func requireRegistered(t *testing.T, registration PluginCapabilityRegistration, digest string, after time.Time) {
	t.Helper()
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, registration.State)
	require.Equal(t, digest, *registration.PackageSha256)
	require.Nil(t, registration.Detail)
	require.False(t, registration.ReconciledAt.Before(after), "reconciled at %s, not before %s", *registration.ReconciledAt, after)
}

func TestCoreRegistersPackageCapabilitiesInEveryEnabledOrganization(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx := t.Context()
	for _, org := range []string{"beta", "gamma"} {
		_, err := db.CreateOrg(ctx, nil, &model.Org{Id: org, CreatedBy: pluginOperator, CreatedAt: time.Now().UTC(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
		require.NoError(t, err)
	}
	iam := operatorIAM(t)
	s := pluginManagerServer(t, db, iam)
	api := newOperatorAPI(t, s, iam)
	item := workflowPlugin(grant, "external-deployment.admit")
	item.Capabilities = append(item.Capabilities, plugins.Capability{ID: "plugin." + item.ID + ".approve", DisplayName: "Approve workflows",
		Description: "Approve a frozen scope.", Category: "Workflow", Level: "write", Scopes: []string{"environment", "project"}})
	declared := registrations(item.ID, item.Capabilities)
	installation := api.activate(item)
	id := installation.InstallationId
	require.Empty(t, iam.replaced(), "activating an installation that serves no organization registers nothing")

	// Enabling an organization registers the served package's declarations
	// there right after the command. The command's response shows the
	// registration as of the command.
	started := time.Now()
	response := api.put(id, "acme", PluginEnablementPutStateEnabled)
	require.Equal(t, PluginCapabilityRegistrationStatePending, response.Organizations[0].CapabilityRegistration.State)
	require.Equal(t, declared, iam.held("acme", item.ID))
	requireRegistered(t, api.capabilityStates(id)["acme"], installation.PackageSha256, started.Truncate(time.Millisecond))
	api.put(id, "beta", PluginEnablementPutStateEnabled)
	require.Equal(t, declared, iam.held("beta", item.ID))
	require.Equal(t, map[string]int{"acme": 1, "beta": 1}, iam.replaced(), "enabling beta changes nothing in acme")
	states := api.capabilityStates(id)
	requireRegistered(t, states["beta"], installation.PackageSha256, started.Truncate(time.Millisecond))

	// A reconciliation with nothing to change only reads IAM, also when IAM
	// lists the scopes in another order.
	reordered := slices.Clone(declared)
	reordered[0].Scopes = []iamapi.PluginCapabilityRegistrationScopes{reordered[0].Scopes[1], reordered[0].Scopes[0]}
	iam.replace("beta", item.ID, reordered)
	before := iam.catalogueReads.Load()
	require.NoError(t, s.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, before+2, iam.catalogueReads.Load(), "one read per enabled organization")
	require.Equal(t, map[string]int{"acme": 1, "beta": 1}, iam.replaced())
	confirmed := api.capabilityStates(id)
	require.True(t, confirmed["acme"].ReconciledAt.After(*states["acme"].ReconciledAt), "the confirmation is recorded")

	// Drift is repaired; another plugin's capabilities and IAM's own
	// permissions are never Core's to change.
	nested := []iamapi.PluginCapabilityRegistration{{Name: "read", DisplayName: "Read", Description: "Nested plugin.", Category: "Nested", Level: iamapi.PluginCapabilityRegistrationLevelRead,
		Scopes: []iamapi.PluginCapabilityRegistrationScopes{iamapi.PluginCapabilityRegistrationScopesOrganization}}}
	iam.replace("acme", item.ID+".nested", nested)
	changed := slices.Clone(declared)
	changed[0].Description = "Edited by hand."
	iam.replace("acme", item.ID, changed[:1])
	require.NoError(t, s.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, declared, iam.held("acme", item.ID))
	require.Equal(t, nested, iam.held("acme", item.ID+".nested"))
	require.Equal(t, map[string]int{"acme": 2, "beta": 1}, iam.replaced())

	// IAM cannot answer: the enablement stands, its registration failed, and
	// the next reconciliation completes it. Other organizations are not
	// reconciled by that command.
	iam.catalogueDown.Store(true)
	api.put(id, "gamma", PluginEnablementPutStateEnabled)
	states = api.capabilityStates(id)
	require.Equal(t, PluginCapabilityRegistrationStateFailed, states["gamma"].State)
	require.Contains(t, *states["gamma"].Detail, "503")
	require.Equal(t, installation.PackageSha256, *states["gamma"].PackageSha256)
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, states["acme"].State)
	require.Error(t, s.PluginManager.ReconcileCapabilities(ctx), "a periodic reconciliation reports what it could not do")
	iam.catalogueDown.Store(false)
	require.NoError(t, s.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, declared, iam.held("gamma", item.ID))
	for org, registration := range api.capabilityStates(id) {
		require.Equal(t, PluginCapabilityRegistrationStateRegistered, registration.State, org)
	}

	// Disabling keeps the catalogue: safety work in acme still needs IAM to
	// check the original actor's capabilities. Removing the enablement keeps
	// it too; Core refuses every effect without an enablement.
	replaced := iam.replaced()
	api.put(id, "acme", PluginEnablementPutStateDisabled)
	require.NoError(t, s.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, declared, iam.held("acme", item.ID))
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, api.capabilityStates(id)["acme"].State)
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodDelete, "/plugin-installations/"+id.String()+"/organizations/acme", uuid.NewString(),
		command(api.revision(), "Remove acme")), http.StatusOK)
	require.NoError(t, s.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, declared, iam.held("acme", item.ID))
	require.Equal(t, replaced, iam.replaced(), "neither disabling nor removing an organization unregisters anything")

	// An upgrade switches every enabled organization to the successor's
	// declarations when the activation switches the package.
	successor := item
	successor.Version = "1.1.0"
	successor.Capabilities = []plugins.Capability{item.Capabilities[0], {ID: "plugin." + item.ID + ".approve", DisplayName: "Approve workflows",
		Description: "Approve a frozen Environment.", Category: "Workflow", Level: "manage", Scopes: []string{"environment"}}}
	manifest, err := json.Marshal(successor.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	decode[PluginPackage](t, api.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: api.revision(), Reason: "Register the successor", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPost, "/plugin-installations/"+id.String()+"/upgrade", uuid.NewString(),
		PluginInstallationUpgrade{ExpectedRevision: api.revision(), Reason: "Upgrade", PackageSha256: digest}), http.StatusOK)
	require.Equal(t, reordered, iam.held("beta", item.ID), "a pending upgrade registers nothing")
	fleet.serve(t, item.Backend, id, digest)
	switched := time.Now()
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPost, "/plugin-installations/"+id.String()+"/actions/activate", uuid.NewString(),
		command(api.revision(), "Switch to the successor")), http.StatusOK)
	for _, org := range []string{"beta", "gamma"} {
		require.Equal(t, registrations(item.ID, successor.Capabilities), iam.held(org, item.ID), org)
		requireRegistered(t, api.capabilityStates(id)[org], digest, switched.Truncate(time.Millisecond))
	}

	// Another replica that starts finds every catalogue in place.
	replaced = iam.replaced()
	other := pluginManagerServer(t, db, iam)
	require.NoError(t, other.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, replaced, iam.replaced())

	// A replica that stopped between switching the package and reconciling
	// left beta's registration at the predecessor: it shows pending until the
	// next reconciliation, which any replica runs.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, model.RecordPluginCapabilityReconciliation(ctx, tx, model.PluginCapabilityReconciliation{PluginID: item.ID, OrgID: "beta",
		PackageSHA256: installation.PackageSha256, ReconciledAt: switched}))
	require.NoError(t, tx.Commit())
	iam.replace("beta", item.ID, declared)
	stale := api.capabilityStates(id)["beta"]
	require.Equal(t, PluginCapabilityRegistrationStatePending, stale.State)
	require.Equal(t, installation.PackageSha256, *stale.PackageSha256, "pending names the package Core last reconciled")
	require.NoError(t, other.PluginManager.ReconcileCapabilities(ctx))
	require.Equal(t, registrations(item.ID, successor.Capabilities), iam.held("beta", item.ID))
	requireRegistered(t, api.capabilityStates(id)["beta"], digest, switched.Truncate(time.Millisecond))
}

func TestCapabilityReconciliationsOfOnePluginTakeTurns(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t)
	ctx := t.Context()
	iam := operatorIAM(t)
	s := pluginManagerServer(t, db, iam)
	api := newOperatorAPI(t, s, iam)
	item := workflowPlugin(grant, "external-deployment.admit")
	installation := api.install(item, grant.OrgID)
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, api.capabilityStates(installation.InstallationId)[grant.OrgID].State)

	successor := item
	successor.Version = "1.1.0"
	successor.Capabilities = []plugins.Capability{item.Capabilities[0]}
	successor.Capabilities[0].Description = "View workflows, their history and their plans."
	manifest, err := json.Marshal(successor.Manifest())
	require.NoError(t, err)
	digest := sha256Hex(manifest)
	decode[PluginPackage](t, api.call(pluginOperator, http.MethodPost, "/plugin-packages", uuid.NewString(),
		PluginPackagePush{ExpectedRevision: api.revision(), Reason: "Register the successor", ManifestSha256: digest, Manifest: manifest}), http.StatusCreated)

	// A reconciliation of the plugin waits for the one in progress.
	holder, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, model.LockPluginCapabilities(ctx, holder, item.ID))
	reads := iam.catalogueReads.Load()
	done := make(chan error, 1)
	go func() { done <- s.PluginManager.ReconcileCapabilities(ctx) }()
	waiting := func() bool {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var count int
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&count))
		return count > 0
	}
	require.Eventually(t, waiting, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, reads, iam.catalogueReads.Load(), "the waiting reconciliation has not asked IAM")

	// The package changes while it waits, as on a replica that switched it
	// and stopped before it reconciled. The waiting reconciliation registers
	// what is served once its turn comes, not what it saw before it waited.
	switched, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = switched.ExecContext(ctx, `UPDATE plugin_installations SET package_sha256 = $1 WHERE installation_uuid = $2`, digest, installation.InstallationId)
	require.NoError(t, err)
	require.NoError(t, switched.Commit())
	require.NoError(t, holder.Rollback())
	require.NoError(t, <-done)
	require.Equal(t, reads+1, iam.catalogueReads.Load())
	require.Equal(t, registrations(item.ID, successor.Capabilities), iam.held(grant.OrgID, item.ID))
	require.Equal(t, digest, *api.capabilityStates(installation.InstallationId)[grant.OrgID].PackageSha256)
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, api.capabilityStates(installation.InstallationId)[grant.OrgID].State)
}
