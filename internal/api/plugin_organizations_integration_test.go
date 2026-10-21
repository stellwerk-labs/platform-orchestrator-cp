//go:build integration

package api

import (
	"crypto/ed25519"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

// recordedBackends stands in for the plugin backend behind the gateway and
// records every call Core delegates to it.
type recordedBackends struct {
	mu    sync.Mutex
	calls []*http.Request
}

func (b *recordedBackends) RoundTrip(request *http.Request) (*http.Response, error) {
	b.mu.Lock()
	b.calls = append(b.calls, request)
	b.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"items":[]}`)), Request: request}, nil
}

func (b *recordedBackends) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *recordedBackends) last() *http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

// organizations is one installation of the workflow plugin enabled for acme
// and beta. Each organization has its own project, Environment, Module and
// active operation grant of the plugin. One Control Plane replica serves the
// operator API and the plugin boundary over HTTP.
type organizations struct {
	t            *testing.T
	db           model.Databaser
	server       *Server
	api          *operatorAPI
	item         plugins.Installation
	installation uuid.UUID
	grants       map[string]*model.ExternalOperationGrant
	backends     *recordedBackends
	assertions   ed25519.PublicKey
}

// newOrganizations seeds acme and beta with operation grants that also
// delegate actions, and enables one installation for both.
func newOrganizations(t *testing.T, actions ...string) *organizations {
	t.Helper()
	db := pluginTestDatabase(t)
	grants := map[string]*model.ExternalOperationGrant{
		"acme": seedPluginOperation(t, db, "acme", actions...),
		"beta": seedPluginOperation(t, db, "beta", actions...),
	}
	iam := operatorIAM(t)
	s := pluginManagerServer(t, db, iam)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	backends := &recordedBackends{}
	s.PluginGateway.SigningKey, s.PluginGateway.Issuer, s.PluginGateway.Client = private, "core", &http.Client{Transport: backends}
	api := newOperatorAPI(t, s, iam)
	item := workflowPlugin(grants["acme"], append([]string{"external-deployment.admit"}, actions...)...)
	installation := api.install(item, "acme")
	o := &organizations{t: t, db: db, server: s, api: api, item: item, installation: installation.InstallationId, grants: grants, backends: backends, assertions: public}
	both := o.enable("beta", PluginEnablementPutStateEnabled, "America/New_York")
	require.Len(t, both.Organizations, 2)
	return o
}

// enable puts org's enablement over the operator API.
func (o *organizations) enable(org string, state PluginEnablementPutState, timezone string) PluginInstallation {
	o.t.Helper()
	return decode[PluginInstallation](o.t, o.api.call(pluginOperator, http.MethodPut, "/plugin-installations/"+o.installation.String()+"/organizations/"+org, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: o.api.revision(), Reason: "Enable " + org, State: state, Timezone: &timezone}), http.StatusOK)
}

// operation is the plugin boundary path of operation in org.
func (o *organizations) operation(org string, operation uuid.UUID) string {
	return "/orgs/" + org + "/plugins/" + o.item.ID + "/operations/" + operation.String()
}

// environment is the Environment of org's operation grant.
func (o *organizations) environment(org string) model.ExternalOperationEnvironment {
	return o.grants[org].Intent.Environments[0]
}

// state digests every row of plugin state Core keeps for org.
func (o *organizations) state(org string) map[string]string {
	o.t.Helper()
	tx, err := o.db.BeginTx(o.t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(o.t, err)
	defer func() { _ = tx.Rollback() }()
	digests := map[string]string{}
	for _, table := range []string{"external_operation_grants", "external_operation_grant_commands", "external_operation_dispatches", "external_module_operations",
		"external_module_operation_receipts", "extension_published_events", "extension_resource_event_consumers", "extension_resource_events", "environment_module_version_pins"} {
		var digest string
		require.NoError(o.t, tx.QueryRowContext(o.t.Context(), `SELECT count(*)::text || ':' || COALESCE(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM `+table+` t WHERE org_id=$1`, org).Scan(&digest))
		digests[table] = digest
	}
	return digests
}

func TestOneInstallationServesEachOrganizationApart(t *testing.T) {
	o := newOrganizations(t, "resource-events.read")
	ctx := t.Context()
	user, service := userid.NewHumanUserId(), uuid.MustParse(o.item.ServiceUserID)
	_, err := o.db.CreateOrg(ctx, nil, &model.Org{Id: "gamma", CreatedBy: pluginOperator, CreatedAt: time.Now().UTC(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)

	read := decode[PluginInstallation](t, o.api.call(pluginOperator, http.MethodGet, "/plugin-installations/"+o.installation.String(), "", nil), http.StatusOK)
	zones := map[string]string{}
	for _, enablement := range read.Organizations {
		require.Equal(t, PluginEnablementStateEnabled, enablement.State)
		zones[enablement.OrgId] = enablement.Timezone
	}
	require.Equal(t, map[string]string{"acme": "Europe/Berlin", "beta": "America/New_York"}, zones, "the installation lists each organization with its own timezone")

	// Discovery: every enabled organization finds the plugin, any other does not.
	for _, org := range []string{"acme", "beta"} {
		page := decode[PluginContributionPage](t, o.api.call(user, http.MethodGet, "/orgs/"+org+"/plugins", "", nil), http.StatusOK)
		require.Len(t, page.Items, 1, org)
		require.Equal(t, o.item.ID, page.Items[0].Id)
		require.Equal(t, PluginAvailabilityStateAvailable, page.Items[0].State, org)
	}
	require.Empty(t, decode[PluginContributionPage](t, o.api.call(user, http.MethodGet, "/orgs/gamma/plugins", "", nil), http.StatusOK).Items)

	// Invoke: the gateway delegates each call with the organization of the
	// request, and a delegation for one organization is no delegation for the
	// other.
	for org, other := range map[string]string{"acme": "beta", "beta": "acme"} {
		response := o.api.call(user, http.MethodPost, "/orgs/"+org+"/plugins/"+o.item.ID+"/invoke", "", PluginInvocation{OperationId: "list"})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		call := o.backends.last()
		require.Equal(t, "/orgs/"+org+"/items", call.URL.Path)
		request := plugincontext.Request{Issuer: "core", Audience: "plugin:" + o.item.ID, PluginID: o.item.ID, OrgID: org, Method: call.Method, Path: call.URL.RequestURI(),
			OperationID: "list", IdempotencyKey: call.Header.Get("Idempotency-Key")}
		_, err := plugincontext.Verify(o.assertions, call.Header.Get(plugincontext.Header), request, time.Now())
		require.NoError(t, err, "the delegation names %s", org)
		request.OrgID = other
		_, err = plugincontext.Verify(o.assertions, call.Header.Get(plugincontext.Header), request, time.Now())
		require.Error(t, err, "a delegation for %s never passes for %s", org, other)
	}
	calls := o.backends.count()
	response := o.api.call(user, http.MethodPost, "/orgs/gamma/plugins/"+o.item.ID+"/invoke", "", PluginInvocation{OperationId: "list"})
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	require.Equal(t, calls, o.backends.count(), "an organization the installation is not enabled for reaches no backend")

	// Journal: each organization is journaled on its own, and each
	// organization's operation reads exactly that organization's events in
	// its timezone.
	for _, org := range []string{"acme", "beta"} {
		journalEnvironmentEvent(t, o.db, org, o.environment(org).EnvironmentID)
	}
	for org, zone := range zones {
		page := decode[ReadPluginResourceEvents200JSONResponse](t, o.api.call(service, http.MethodGet, o.operation(org, o.grants[org].OperationID)+"/resource-events?limit=10", "", nil), http.StatusOK)
		require.Len(t, page.Events, 1, org)
		require.Equal(t, o.environment(org).EnvironmentID, *page.Events[0].EnvironmentUuid, "%s reads only its own Environment's event", org)
		require.Equal(t, zone, page.OrganizationTimezone)
	}
}

func TestEntitlementsBindEachOrganizationOfOneInstallation(t *testing.T) {
	f := newUnimportedPluginEntitlementAPIFixture(t)
	ctx := t.Context()
	beta, err := f.db.CreateOrg(ctx, nil, &model.Org{Id: "beta", CreatedBy: pluginOperator, CreatedAt: time.Now().UTC(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	registry := &plugins.InstallationRegistry{Entitlements: f.adapter}
	iam := operatorIAM(t)
	manager := testPluginManager(t, f.db, registry, f.adapter, iam)
	require.NoError(t, manager.Refresh(ctx))
	f.server.PluginManager, f.server.PluginEntitlements, f.server.PluginGateway = manager, f.adapter, &plugins.Gateway{Registry: registry}
	api := newOperatorAPI(t, f.server, iam)
	enabled := "UTC"
	decode[PluginInstallation](t, api.call(pluginOperator, http.MethodPut, "/plugin-installations/"+f.item.InstallationID+"/organizations/beta", uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: api.revision(), Reason: "Enable beta", State: PluginEnablementPutStateEnabled, Timezone: &enabled}), http.StatusOK)
	served := func(org string) (plugins.Installation, plugins.AvailabilityState) {
		t.Helper()
		item, descriptor, found := registry.Installation(ctx, org, f.item.ID)
		require.True(t, found, org)
		return item, descriptor.State
	}
	effect := func(org string) error {
		item, _ := served(org)
		tx, err := f.server.pluginEffectDatabase(org, item, "effect.commit", false).BeginTx(ctx, nil)
		if err == nil {
			require.NoError(t, tx.Rollback())
		}
		return err
	}
	importGrants := func(reason string, grants ...[]byte) {
		t.Helper()
		imported := decode[PluginEntitlementBundleImport](t, api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", uuid.NewString(),
			PluginEntitlementBundleImportRequest{ExpectedRevision: api.revision(), Reason: reason, Bundle: f.bundle(t, grants...)}), http.StatusOK)
		require.Empty(t, imported.Diagnostics)
		require.Equal(t, len(grants), imported.ImportedGrants)
	}

	for _, org := range []string{"acme", "beta"} {
		_, state := served(org)
		require.Equal(t, plugins.StateNotEntitled, state, org)
	}

	// A grant names one organization of the installation and licenses only it.
	importGrants("Import the licence of acme", f.grant.Envelope)
	_, state := served("acme")
	require.Equal(t, plugins.StateAvailable, state)
	_, state = served("beta")
	require.Equal(t, plugins.StateNotEntitled, state, "acme's grant does not license beta")
	require.NoError(t, effect("acme"))
	requirePlatformEntitlementCode(t, effect("beta"), "plugin_not_entitled")

	// beta's own grant licenses beta and leaves acme's binding as it was.
	installation := uuid.MustParse(f.item.InstallationID)
	importGrants("Import the licence of beta", f.signGrantForOrganization(t, installation, beta.Uuid, uuid.New(), 1, f.base+10, f.base+20))
	for _, org := range []string{"acme", "beta"} {
		_, state := served(org)
		require.Equal(t, plugins.StateAvailable, state, org)
		require.NoError(t, effect(org), org)
	}
}

func TestOrganizationStateNeverCrossesIntoAnotherOrganization(t *testing.T) {
	o := newOrganizations(t, "resource-events.read", "events.publish", "pin.override.begin")
	ctx := t.Context()
	acme, beta := o.grants["acme"], o.grants["beta"]
	service, checker, user := o.item.ServiceUserID, o.item.AdmissionCheckerID, userid.NewHumanUserId()

	// The Data Plane keeps Deployment material per organization: this one
	// holds a Deployment of acme and records which organization each request
	// named in its path and in Core's signed delegation.
	deployment := uuid.New()
	var named []string
	var dataPlane sync.Mutex
	dp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		org := strings.Split(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/")[0]
		claims, err := plugincontext.Verify(o.assertions, r.Header.Get(plugincontext.Header), plugincontext.Request{Issuer: "core", Audience: "platform-orchestrator-dp",
			PluginID: o.item.ID, OrgID: org, OperationID: plugins.DataPlaneLogsRead, Method: r.Method, Path: r.URL.EscapedPath(), IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now())
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		dataPlane.Lock()
		named = append(named, org+"/"+claims.OrgID)
		dataPlane.Unlock()
		env := o.environment("acme")
		if r.URL.Path != "/orgs/acme/plugins/"+o.item.ID+"/projects/"+env.ProjectID.String()+"/envs/"+env.EnvironmentID.String()+"/external-deployments/"+deployment.String()+"/logs" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"deployment_material_not_found","message":"No such Deployment"}`))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("acme deployment log\n"))
	}))
	t.Cleanup(dp.Close)
	var err error
	o.server.PluginDataPlane, err = plugins.NewDataPlanePort(dp.URL, o.server.PluginGateway, true)
	require.NoError(t, err)
	material := func(org string) *httptest.ResponseRecorder {
		env := o.environment("acme")
		return o.api.call(user, http.MethodGet, "/orgs/"+org+"/plugins/"+o.item.ID+"/projects/"+env.ProjectID.String()+"/envs/"+env.EnvironmentID.String()+"/deployments/"+deployment.String()+"/logs", "", nil)
	}

	// acme's state: a resource event cursor, a published event and a Pin
	// receipt of acme's operation.
	journalEnvironmentEvent(t, o.db, "acme", o.environment("acme").EnvironmentID)
	page := decode[ReadPluginResourceEvents200JSONResponse](t, o.api.call(uuid.MustParse(service), http.MethodGet, o.operation("acme", acme.OperationID)+"/resource-events?limit=10", "", nil), http.StatusOK)
	require.Len(t, page.Events, 1)
	cursor := page.NextCursor
	event := PublishPluginEventJSONRequestBody{EventId: uuid.New(), GrantFence: acme.Revision, Type: "plugin." + o.item.ID + ".completed", OccurredAt: time.Now().UTC(), Data: map[string]any{"status": "Succeeded"}}
	published := o.api.call(uuid.MustParse(service), http.MethodPost, o.operation("acme", acme.OperationID)+"/events", "", event)
	require.Equal(t, http.StatusNoContent, published.Code, published.Body.String())
	env, module := o.environment("acme"), acme.Intent.Modules[0]
	tx, err := o.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	pin, err := o.db.CreateEnvironmentModuleVersionPin(ctx, tx, "acme", env.ProjectID, "project", env.EnvironmentID, "development", module, *acme.BaselineDefaults[module.String()], acme.ActorID, "user", "Protect the baseline", nil, false)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	pins := model.ExternalPinCommand{OrgID: "acme", PluginID: o.item.ID, OperationID: acme.OperationID, GrantFence: acme.Revision, ActorID: acme.ActorID, RequestID: "acme-override", Action: "begin", Reason: "Explicit override",
		Targets: []model.ExternalPinTarget{{PinID: pin.ID, ExpectedResourceVersion: pin.ResourceVersion, TargetVersionUUID: acme.Intent.TargetVersions[module.String()]}}}
	_, err = model.ApplyExternalPinCommand(ctx, o.db, pins, nil)
	require.NoError(t, err)
	receipt := func(org string, operation uuid.UUID, fence int64) string {
		return o.operation(org, operation) + "/pins/receipts/" + pins.RequestID + "?grant_fence=" + strconv.FormatInt(fence, 10)
	}
	require.Equal(t, http.StatusOK, o.api.call(uuid.MustParse(service), http.MethodGet, receipt("acme", acme.OperationID, acme.Revision), "", nil).Code)
	require.Equal(t, http.StatusOK, material("acme").Code, "acme's own material is served in acme")

	// Under beta, every piece of acme's state is refused and nothing changes
	// in either organization.
	acmeState, betaState := o.state("acme"), o.state("beta")
	dispatch := PluginDispatchCheck{Action: ExternalDeploymentAdmit, Fence: acme.Revision, DispatchId: "acme-dispatch", ProjectUuid: env.ProjectID, EnvironmentUuid: env.EnvironmentID, RequestDigest: strings.Repeat("c", 64)}
	for name, refusal := range map[string]struct {
		actor  string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		"acme's operation grant":                   {service, http.MethodGet, o.operation("beta", acme.OperationID), nil, http.StatusNotFound, "plugin_operation_not_found"},
		"acme's resource events":                   {service, http.MethodGet, o.operation("beta", acme.OperationID) + "/resource-events", nil, http.StatusNotFound, "plugin_operation_not_found"},
		"acme's cursor with beta's operation":      {service, http.MethodGet, o.operation("beta", beta.OperationID) + "/resource-events?after=" + cursor, nil, http.StatusBadRequest, "plugin_cursor_invalid"},
		"acme's published event":                   {service, http.MethodPost, o.operation("beta", acme.OperationID) + "/events", event, http.StatusNotFound, "plugin_operation_not_found"},
		"acme's Pin receipt":                       {service, http.MethodGet, receipt("beta", acme.OperationID, acme.Revision), nil, http.StatusNotFound, "plugin_operation_not_found"},
		"acme's Pin receipt with beta's operation": {service, http.MethodGet, receipt("beta", beta.OperationID, beta.Revision), nil, http.StatusNotFound, "plugin_pin_receipt_not_found"},
		"acme's dispatch at the admission checker": {checker, http.MethodPost, o.operation("beta", acme.OperationID) + "/check", dispatch, http.StatusNotFound, "plugin_operation_not_found"},
		"acme's Deployment material":               {user.String(), http.MethodGet, "", nil, http.StatusNotFound, "deployment_material_unavailable"},
	} {
		var response *httptest.ResponseRecorder
		if refusal.path == "" {
			response = material("beta")
		} else {
			response = o.api.call(uuid.MustParse(refusal.actor), refusal.method, refusal.path, "", refusal.body)
		}
		require.Equal(t, refusal.status, response.Code, "%s under beta: %s", name, response.Body.String())
		require.Contains(t, response.Body.String(), `"error":"`+refusal.code, name)
	}
	require.Equal(t, acmeState, o.state("acme"), "nothing changed in acme")
	require.Equal(t, betaState, o.state("beta"), "nothing changed in beta")
	require.Equal(t, []string{"acme/acme", "beta/beta"}, named, "Core asks the Data Plane only for the organization of the request")

	// acme's state still serves acme.
	page = decode[ReadPluginResourceEvents200JSONResponse](t, o.api.call(uuid.MustParse(service), http.MethodGet, o.operation("acme", acme.OperationID)+"/resource-events?after="+cursor, "", nil), http.StatusOK)
	require.NotEmpty(t, page.Events, "the Pin override journaled acme's Pin changes")
	for _, event := range page.Events {
		require.Equal(t, env.EnvironmentID, *event.EnvironmentUuid, "acme's cursor continues in acme's journal")
	}
	require.Equal(t, http.StatusOK, o.api.call(uuid.MustParse(service), http.MethodGet, receipt("acme", acme.OperationID, acme.Revision), "", nil).Code)
}

func TestDisablingOneOrganizationLeavesTheOtherServed(t *testing.T) {
	o := newOrganizations(t)
	ctx := t.Context()
	other := pluginManagerServer(t, o.db, o.api.iam)
	replicas := map[string]*Server{"writer": o.server, "other": other}
	// Each replica authorizes an effect from its snapshot; the effect then
	// re-verifies the enablement in its own transaction.
	snapshot := map[string]plugins.Installation{}
	for name, replica := range replicas {
		for _, org := range []string{"acme", "beta"} {
			item, _, found := replica.PluginGateway.Registry.Installation(ctx, org, o.item.ID)
			require.True(t, found, "%s serves %s", name, org)
			snapshot[name+"/"+org] = item
		}
	}
	effect := func(replica, org string, safety bool) error {
		t.Helper()
		tx, err := replicas[replica].pluginEffectDatabase(org, snapshot[replica+"/"+org], "external-deployment.admit", safety).BeginTx(ctx, nil)
		if err == nil {
			require.NoError(t, tx.Rollback())
		}
		return err
	}
	for name := range replicas {
		for _, org := range []string{"acme", "beta"} {
			require.NoError(t, effect(name, org, false), "%s serves %s", name, org)
		}
	}

	o.enable("acme", PluginEnablementPutStateDisabled, "Europe/Berlin")
	_, stale, _ := other.PluginGateway.Registry.Installation(ctx, "acme", o.item.ID)
	require.Equal(t, plugins.StateAvailable, stale.State, "the other replica has not refreshed its snapshot yet")
	for name := range replicas {
		requireEffectCode(t, effect(name, "acme", false), "plugin_not_enabled")
		require.NoError(t, effect(name, "acme", true), "%s keeps acme's safety work", name)
		require.NoError(t, effect(name, "beta", false), "%s keeps serving beta", name)
	}

	go other.PluginManager.Run(ctx, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		acme, _ := other.PluginGateway.Registry.Get(ctx, "acme", o.item.ID)
		beta, _ := other.PluginGateway.Registry.Get(ctx, "beta", o.item.ID)
		return acme.State == plugins.StateNotEnabled && beta.State == plugins.StateAvailable
	}, 2*time.Second, 10*time.Millisecond, "reads follow within the refresh bound")

	decode[PluginInstallation](t, o.api.call(pluginOperator, http.MethodDelete, "/plugin-installations/"+o.installation.String()+"/organizations/acme", uuid.NewString(),
		command(o.api.revision(), "Remove acme")), http.StatusOK)
	for name := range replicas {
		requireEffectCode(t, effect(name, "acme", false), "plugin_not_installed")
		requireEffectCode(t, effect(name, "acme", true), "plugin_not_installed")
		require.NoError(t, effect(name, "beta", false), "%s keeps serving beta", name)
	}
}
