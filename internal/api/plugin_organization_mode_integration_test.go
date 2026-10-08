//go:build integration

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

// setOrganizationMode sets the organization mode of installation over the
// operator API.
func (a *operatorAPI) setOrganizationMode(installation uuid.UUID, mode PluginOrganizationMode, timezone string) PluginInstallation {
	a.t.Helper()
	return decode[PluginInstallation](a.t, a.call(pluginOperator, http.MethodPut, "/plugin-installations/"+installation.String()+"/organization-mode", uuid.NewString(),
		PluginOrganizationModePut{ExpectedRevision: a.revision(), Reason: "Serve " + string(mode) + " organizations", Mode: mode, DefaultTimezone: &timezone}), http.StatusOK)
}

// createOrganization creates org over the internal API, as the platform's
// organization provisioning does.
func (a *operatorAPI) createOrganization(org string) InternalOrganization {
	a.t.Helper()
	return decode[InternalOrganization](a.t, a.call(userid.InternalSystemUuid, http.MethodPost, "/internal/orgs", "", InternalOrganizationCreateBody{Id: &org}), http.StatusCreated)
}

// enablementsOf returns each organization's enablement as the operator reads
// the installation.
func (a *operatorAPI) enablementsOf(installation uuid.UUID) map[string]PluginEnablement {
	a.t.Helper()
	read := decode[PluginInstallation](a.t, a.call(pluginOperator, http.MethodGet, "/plugin-installations/"+installation.String(), "", nil), http.StatusOK)
	result := map[string]PluginEnablement{}
	for _, enablement := range read.Organizations {
		result[enablement.OrgId] = enablement
	}
	return result
}

// discovered returns the availability org's members discover for plugin.
func (a *operatorAPI) discovered(org, plugin string) PluginAvailabilityState {
	a.t.Helper()
	page := decode[PluginContributionPage](a.t, a.call(userid.NewHumanUserId(), http.MethodGet, "/orgs/"+org+"/plugins", "", nil), http.StatusOK)
	for _, item := range page.Items {
		if item.Id == plugin {
			return item.State
		}
	}
	return PluginAvailabilityStateNotInstalled
}

// lastAudit returns the newest Plugin Manager audit event.
func (a *operatorAPI) lastAudit() PluginAuditEvent {
	a.t.Helper()
	page := decode[PluginAuditEventPage](a.t, a.call(pluginOperator, http.MethodGet, "/plugin-audit-events?after=0&limit=200", "", nil), http.StatusOK)
	return page.Items[len(page.Items)-1]
}

// rowValues returns the text of one column per query, each query reading
// exactly one row by org.
func rowValues(t *testing.T, db model.Databaser, org string, queries ...string) []string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	values := []string{}
	for _, query := range queries {
		var value string
		require.NoError(t, tx.QueryRowContext(t.Context(), query, org).Scan(&value), query)
		values = append(values, value)
	}
	return values
}

func TestAnOrganizationCreatedAfterActivationIsServedFromItsCreation(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "resource-events.read")
	iam := operatorIAM(t)
	api := newOperatorAPI(t, pluginManagerServer(t, db, iam), iam)
	item := workflowPlugin(grant, "resource-events.read")
	id := api.install(item, grant.OrgID).InstallationId
	all := api.setOrganizationMode(id, PluginOrganizationModeAll, "Europe/Vienna")
	require.Equal(t, PluginOrganizationModeAll, all.OrganizationMode)
	require.Equal(t, PluginOrganizationDefaults{Timezone: "Europe/Vienna", ApprovalPolicies: map[string]string{}}, all.OrganizationDefaults)

	created := api.createOrganization("newcomer")
	require.Equal(t, "newcomer", created.Id)

	// The enablement, its resource journal and the organization are one
	// transaction: their rows carry the same transaction ID.
	xmins := rowValues(t, db, "newcomer", `SELECT xmin::text FROM orgs WHERE id=$1`, `SELECT xmin::text FROM plugin_enablements WHERE org_id=$1`,
		`SELECT xmin::text FROM extension_resource_event_journal_state WHERE org_id=$1`)
	require.Equal(t, []string{xmins[0], xmins[0], xmins[0]}, xmins, "enabled in the organization's creation transaction")
	require.Equal(t, []string{"true", "0"}, rowValues(t, db, "newcomer", `SELECT journaling::text FROM extension_resource_event_journal_state WHERE org_id=$1`,
		`SELECT gap_cursor::text FROM extension_resource_event_journal_state WHERE org_id=$1`), "the journal starts at the creation, without a gap")

	enablement := api.enablementsOf(id)["newcomer"]
	require.Equal(t, PluginEnablementStateEnabled, enablement.State)
	require.Equal(t, "Europe/Vienna", enablement.Timezone)
	require.Empty(t, enablement.ApprovalPolicies)
	require.Equal(t, userid.InternalSystemUuid, enablement.CreatedBy, "Core enabled it on its own")
	require.Equal(t, PluginCapabilityRegistrationStateRegistered, enablement.CapabilityRegistration.State, "capabilities are registered right after the creation")
	require.Equal(t, registrations(item.ID, item.Capabilities), iam.held("newcomer", item.ID))
	audit := api.lastAudit()
	require.Equal(t, *all.Revision+1, audit.Revision)
	require.Equal(t, userid.InternalSystemUuid, audit.Actor)
	require.Equal(t, "plugin_enablement.put", audit.Action)
	require.Equal(t, "newcomer", ref.DerefOr(audit.OrgId, ""))
	require.Equal(t, id, *audit.InstallationId)
	require.Contains(t, audit.Reason, "Organization newcomer was created")
	require.Equal(t, PluginAvailabilityStateAvailable, api.discovered("newcomer", item.ID), "this replica serves it at once")

	// Its first change is journaled from cursor one on.
	journalEnvironmentEvent(t, db, "newcomer", uuid.New())
	require.Equal(t, []string{"1", "1"}, rowValues(t, db, "newcomer", `SELECT cursor::text FROM extension_resource_event_heads WHERE org_id=$1`,
		`SELECT count(*)::text FROM extension_resource_events WHERE org_id=$1`))
}

func TestAFailedOrganizationCreationTakesItsPluginEnablementsAlong(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "resource-events.read")
	iam := operatorIAM(t)
	s := pluginManagerServer(t, db, iam)
	api := newOperatorAPI(t, s, iam)
	item := workflowPlugin(grant, "resource-events.read")
	id := api.install(item, grant.OrgID).InstallationId
	revision := *api.setOrganizationMode(id, PluginOrganizationModeAll, "UTC").Revision

	// IAM cannot add the founder, so Core removes the organization it
	// already committed, and the enablement Core created with it first.
	iam.membershipsDown.Store(true)
	response := api.call(userid.NewHumanUserId(), http.MethodPost, "/orgs", "", nil)
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())

	page := decode[PluginAuditEventPage](t, api.call(pluginOperator, http.MethodGet, "/plugin-audit-events?after=0&limit=200", "", nil), http.StatusOK)
	created, discarded := page.Items[len(page.Items)-2], page.Items[len(page.Items)-1]
	org := ref.DerefOr(created.OrgId, "")
	require.NotEmpty(t, org)
	require.Equal(t, []string{"plugin_enablement.put", "plugin_enablement.remove"}, []string{created.Action, discarded.Action})
	require.Equal(t, []uuid.UUID{userid.InternalSystemUuid, userid.InternalSystemUuid}, []uuid.UUID{created.Actor, discarded.Actor})
	require.Equal(t, org, ref.DerefOr(discarded.OrgId, ""))
	require.Equal(t, []int64{revision + 1, revision + 2}, []int64{created.Revision, discarded.Revision})
	require.Contains(t, discarded.Reason, "creation failed")
	_, err := db.GetOrg(t.Context(), nil, org)
	_, missing := model.IsErrNotFound(err)
	require.True(t, missing, "the organization is gone: %v", err)
	require.Equal(t, []string{"0", "0", "0"}, rowValues(t, db, org, `SELECT count(*)::text FROM plugin_enablements WHERE org_id=$1`,
		`SELECT count(*)::text FROM extension_resource_event_journal_state WHERE org_id=$1`, `SELECT count(*)::text FROM plugin_capability_registrations WHERE org_id=$1`))
	require.NotContains(t, api.enablementsOf(id), org)
	require.Empty(t, iam.held(org, item.ID), "nothing was registered in IAM for it")

	// The next organization is served as before.
	iam.membershipsDown.Store(false)
	decode[Organization](t, api.call(userid.NewHumanUserId(), http.MethodPost, "/orgs", "", nil), http.StatusCreated)
	require.Len(t, api.enablementsOf(id), 2)
}

func TestAnOrganizationThatOptedOutStaysDisabledAcrossModeSwitchesAndRestarts(t *testing.T) {
	db, grant := pluginDeploymentBoundaryDatabase(t, "resource-events.read")
	ctx := t.Context()
	iam := operatorIAM(t)
	api := newOperatorAPI(t, pluginManagerServer(t, db, iam), iam)
	item := workflowPlugin(grant, "resource-events.read")
	id := api.install(item, grant.OrgID).InstallationId
	api.createOrganization("beta")
	require.NotContains(t, api.enablementsOf(id), "beta", "selected serves only the chosen organizations")
	api.setOrganizationMode(id, PluginOrganizationModeAll, "UTC")
	require.Equal(t, PluginEnablementStateEnabled, api.enablementsOf(id)["beta"].State)
	api.put(id, "beta", PluginEnablementPutStateDisabled)
	optedOut := api.enablementsOf(id)["beta"]
	require.Equal(t, PluginEnablementStateDisabled, optedOut.State)
	stays := func(api *operatorAPI, why string) {
		t.Helper()
		enablement := api.enablementsOf(id)["beta"]
		require.Equal(t, PluginEnablementStateDisabled, enablement.State, why)
		require.Equal(t, optedOut.UpdatedAt, enablement.UpdatedAt, "%s: Core never wrote beta's enablement again", why)
		require.Equal(t, PluginAvailabilityStateNotEnabled, api.discovered("beta", item.ID), why)
	}

	api.setOrganizationMode(id, PluginOrganizationModeSelected, "UTC")
	stays(api, "switching to selected")
	api.setOrganizationMode(id, PluginOrganizationModeAll, "UTC")
	stays(api, "switching to all again")
	api.setOrganizationMode(id, PluginOrganizationModeAll, "Asia/Tokyo")
	stays(api, "changing the defaults")
	api.createOrganization("gamma")
	stays(api, "creating another organization")
	require.Equal(t, PluginEnablementStateEnabled, api.enablementsOf(id)["gamma"].State)

	// A restarted replica runs Core's start-up steps over the same database.
	restarted := pluginManagerServer(t, db, iam)
	require.NoError(t, model.ReconcileExtensionResourceJournal(ctx, db, true))
	require.NoError(t, restarted.PluginManager.ReconcileCapabilities(ctx))
	after := newOperatorAPI(t, restarted, iam)
	stays(after, "after a restart")
	after.setOrganizationMode(id, PluginOrganizationModeAll, "UTC")
	after.createOrganization("delta")
	stays(after, "after a restart, a mode change and a creation")
	descriptor, found := restarted.PluginGateway.Registry.Get(ctx, "delta", item.ID)
	require.True(t, found)
	require.Equal(t, plugins.StateAvailable, descriptor.State)
}
