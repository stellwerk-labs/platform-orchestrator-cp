//go:build integration

package api

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// signInstallationGrant signs a grant of this plugin for installation without
// an organization: it covers every organization of the installation, up to
// limit enabled organizations when limit is not zero.
func (f *pluginEntitlementAPIFixture) signInstallationGrant(t *testing.T, installation, issuance uuid.UUID, sequence, expires, grace int64, limit uint64) []byte {
	t.Helper()
	if sequence < 1 || f.base < 10 || expires < 0 || grace < 0 {
		t.Fatal("signed grant fixture requires a positive sequence and nonnegative timestamps")
		return nil
	}
	limits := map[string]uint64{}
	if limit != 0 {
		limits["organizations"] = limit
	}
	return f.sign(t, map[uint64]any{1: uint64(1), 2: "grant", 3: "development.example", 4: issuance[:], 5: uint64(sequence), 6: f.item.ID, 7: installation[:], 9: []string{"feature-a"}, 10: limits, 11: uint64(f.base - 10), 12: uint64(f.base - 5), 13: uint64(expires), 14: uint64(grace), 15: "development"})
}

// licensedOrganizations is the licensed workflow plugin, installed and
// enabled for acme, behind one Control Plane replica that serves the operator
// API, discovery, the plugin boundary and organization creation.
type licensedOrganizations struct {
	*pluginEntitlementAPIFixture
	t        *testing.T
	api      *operatorAPI
	registry *plugins.InstallationRegistry
	id       uuid.UUID
	path     string
}

func newLicensedOrganizations(t *testing.T) *licensedOrganizations {
	t.Helper()
	f := newUnimportedPluginEntitlementAPIFixture(t)
	registry := &plugins.InstallationRegistry{Entitlements: f.adapter}
	iam := operatorIAM(t)
	manager := testPluginManager(t, f.db, registry, f.adapter, iam)
	require.NoError(t, manager.Refresh(t.Context()))
	f.server.PluginManager, f.server.PluginEntitlements, f.server.PluginGateway, f.server.Publisher = manager, f.adapter, &plugins.Gateway{Registry: registry}, new(hmessaging.RecordingPublisher)
	id := uuid.MustParse(f.item.InstallationID)
	return &licensedOrganizations{pluginEntitlementAPIFixture: f, t: t, api: newOperatorAPI(t, f.server, iam), registry: registry, id: id, path: "/plugin-installations/" + id.String()}
}

func (l *licensedOrganizations) read() PluginInstallation {
	l.t.Helper()
	return decode[PluginInstallation](l.t, l.api.call(pluginOperator, http.MethodGet, l.path, "", nil), http.StatusOK)
}

// grantOf returns the grant the installation read shows as covering org.
func (l *licensedOrganizations) grantOf(installation PluginInstallation, org string) *PluginEntitlementGrant {
	l.t.Helper()
	for _, enablement := range installation.Organizations {
		if enablement.OrgId == org {
			return enablement.EntitlementGrant
		}
	}
	l.t.Fatalf("%s is not enabled", org)
	return nil
}

// served returns what org's members discover: availability and the scope of
// the grant that covers the organization.
func (l *licensedOrganizations) served(org string) (PluginAvailabilityState, *PluginGrantScope) {
	l.t.Helper()
	page := decode[PluginContributionPage](l.t, l.api.call(pluginOperator, http.MethodGet, "/orgs/"+org+"/plugins", "", nil), http.StatusOK)
	require.Len(l.t, page.Items, 1, org)
	return page.Items[0].State, page.Items[0].Entitlement.GrantScope
}

// effect begins a licensed forward effect of the plugin in org.
func (l *licensedOrganizations) effect(org string) error {
	l.t.Helper()
	item, _, found := l.registry.Installation(l.t.Context(), org, l.item.ID)
	require.True(l.t, found, org)
	tx, err := l.server.pluginEffectDatabase(org, item, "effect.commit", false).BeginTx(l.t.Context(), nil)
	if err == nil {
		require.NoError(l.t, tx.Rollback())
	}
	return err
}

func (l *licensedOrganizations) importGrants(reason string, grants ...[]byte) {
	l.t.Helper()
	imported := decode[PluginEntitlementBundleImport](l.t, l.api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", uuid.NewString(),
		PluginEntitlementBundleImportRequest{ExpectedRevision: l.api.revision(), Reason: reason, Bundle: l.bundle(l.t, grants...)}), http.StatusOK)
	require.Empty(l.t, imported.Diagnostics)
	require.Equal(l.t, len(grants), imported.ImportedGrants)
}

func (l *licensedOrganizations) enable(org string, state PluginEnablementPutState) *httptest.ResponseRecorder {
	return l.api.call(pluginOperator, http.MethodPut, l.path+"/organizations/"+org, uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: l.api.revision(), Reason: "Change " + org, State: state})
}

func grantScope(value PluginGrantScope) *PluginGrantScope { return &value }

func TestAnInstallationWideGrantCoversNewOrganizationsAndAnOrganizationGrantWins(t *testing.T) {
	l := newLicensedOrganizations(t)
	before := l.read()
	require.NotNil(t, before.Entitlement, "the package requires a licence")
	require.Nil(t, before.Entitlement.InstallationGrant)
	require.Nil(t, l.grantOf(before, "acme"), "no grant covers acme yet")
	state, covering := l.served("acme")
	require.Equal(t, PluginAvailabilityStateNotEntitled, state)
	require.Nil(t, covering)

	// A grant without an organization fits the installation before any
	// organization is enabled.
	decode[PluginInstallation](t, l.api.call(pluginOperator, http.MethodDelete, l.path+"/organizations/acme", uuid.NewString(), command(l.api.revision(), "Start without organizations")), http.StatusOK)
	wide := uuid.New()
	l.importGrants("Import the installation's licence", l.signInstallationGrant(t, l.id, wide, 1, l.base+10, l.base+20, 0))
	unused := l.read()
	require.Equal(t, PluginGrantScopeInstallation, unused.Entitlement.InstallationGrant.Scope)
	require.Equal(t, wide, unused.Entitlement.InstallationGrant.IssuanceId)
	require.Equal(t, PluginEntitlementGrantStateValid, unused.Entitlement.InstallationGrant.State)
	require.Nil(t, unused.Entitlement.InstallationGrant.OrganizationLimit)
	require.Zero(t, unused.Entitlement.EnabledOrganizations)
	decode[PluginInstallation](t, l.enable("acme", PluginEnablementPutStateEnabled), http.StatusOK)
	licensed := l.read()
	require.Equal(t, licensed.Entitlement.InstallationGrant, l.grantOf(licensed, "acme"), "acme has no grant of its own")
	state, covering = l.served("acme")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeInstallation), covering)
	require.NoError(t, l.effect("acme"))

	// It covers an organization created later, which mode all enables.
	l.api.setOrganizationMode(l.id, PluginOrganizationModeAll, "UTC")
	l.api.createOrganization("beta")
	state, covering = l.served("beta")
	require.Equal(t, PluginAvailabilityStateAvailable, state, "the installation-wide grant covers an organization without a grant of its own")
	require.Equal(t, grantScope(PluginGrantScopeInstallation), covering)
	require.NoError(t, l.effect("beta"))

	// An expired organisation grant falls back to the current installation
	// grant. Operator status, discovery and effect admission agree.
	acme, err := l.db.GetOrg(t.Context(), nil, "acme")
	require.NoError(t, err)
	l.importGrants("Import acme's own licence", l.signGrantForOrganization(t, l.id, acme.Uuid, uuid.New(), 1, l.base-3, l.base-2))
	own := l.read()
	require.Equal(t, PluginGrantScopeInstallation, l.grantOf(own, "acme").Scope)
	require.Equal(t, PluginEntitlementGrantStateValid, l.grantOf(own, "acme").State)
	require.Equal(t, PluginGrantScopeInstallation, l.grantOf(own, "beta").Scope)
	state, covering = l.served("acme")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeInstallation), covering)
	require.NoError(t, l.effect("acme"))
	state, covering = l.served("beta")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeInstallation), covering)
	require.NoError(t, l.effect("beta"))

	// The plugin's own licence preflight names the covering grant too.
	for org, want := range map[string]PluginGrantScope{"acme": PluginGrantScopeInstallation, "beta": PluginGrantScopeInstallation} {
		preflight := decode[PluginCapability](t, l.api.call(uuid.MustParse(l.item.ServiceUserID), http.MethodGet, "/orgs/"+org+"/plugins/"+l.item.ID+"/entitlement", "", nil), http.StatusOK)
		require.Equal(t, grantScope(want), preflight.Entitlement.GrantScope, org)
	}

	// An uninstalled installation keeps its licence but takes no new grant
	// until it is reinstalled.
	releaseOperationGrants(t, l.db, l.item.ID)
	decode[PluginInstallation](t, l.api.call(pluginOperator, http.MethodPost, l.path+"/actions/uninstall", uuid.NewString(), command(l.api.revision(), "Uninstall")), http.StatusOK)
	require.Equal(t, wide, l.read().Entitlement.InstallationGrant.IssuanceId)
	rejected := decode[PluginEntitlementBundleImport](t, l.api.call(pluginOperator, http.MethodPost, "/plugin-entitlement-bundles", uuid.NewString(),
		PluginEntitlementBundleImportRequest{ExpectedRevision: l.api.revision(), Reason: "Renew while uninstalled", Bundle: l.bundle(t, l.signInstallationGrant(t, l.id, uuid.New(), 2, l.base+10, l.base+20, 0))}), http.StatusOK)
	require.Zero(t, rejected.ImportedGrants)
	require.Equal(t, []string{"plugin_entitlement_grant_rejected"}, []string{rejected.Diagnostics[0].Code})
}

func TestTheOrganizationLimitRefusesEnablementsBeyondIt(t *testing.T) {
	l := newLicensedOrganizations(t)
	l.importGrants("Import the installation's licence for two organizations", l.signInstallationGrant(t, l.id, uuid.New(), 1, l.base+10, l.base+20, 2))
	licensed := l.read()
	require.Equal(t, int64(2), *licensed.Entitlement.InstallationGrant.OrganizationLimit)
	require.Equal(t, 1, licensed.Entitlement.EnabledOrganizations)
	l.api.createOrganization("beta")
	all := l.api.setOrganizationMode(l.id, PluginOrganizationModeAll, "UTC")
	require.Equal(t, 2, all.Entitlement.EnabledOrganizations)
	beta, err := l.db.GetOrg(t.Context(), nil, "beta")
	require.NoError(t, err)
	l.importGrants("License beta separately", l.signGrantForOrganization(t, l.id, beta.Uuid, uuid.New(), 1, l.base+10, l.base+20))
	require.Equal(t, 2, l.read().Entitlement.EnabledOrganizations, "an organization's own licence does not free a limit slot")

	// The limit is reached: a new organization is still created, without an
	// enablement, and waits as pending.
	revision := l.api.revision()
	l.api.createOrganization("gamma")
	limited := l.read()
	require.Equal(t, revision, *limited.Revision, "Core changed nothing for gamma")
	require.Equal(t, []string{"gamma"}, *limited.PendingOrganizations)
	require.Len(t, limited.Organizations, 2)
	requireErrorCode(t, l.enable("gamma", PluginEnablementPutStateEnabled), http.StatusConflict, "plugin_organization_limit")
	requireErrorCode(t, l.api.call(pluginOperator, http.MethodPut, l.path+"/organization-mode", uuid.NewString(),
		PluginOrganizationModePut{ExpectedRevision: l.api.revision(), Reason: "Serve every organization", Mode: PluginOrganizationModeAll}), http.StatusConflict, "plugin_organization_limit")
	require.Equal(t, revision, l.api.revision(), "the refusals change nothing")

	// A disabled organization does not count.
	decode[PluginInstallation](t, l.enable("beta", PluginEnablementPutStateDisabled), http.StatusOK)
	swapped := decode[PluginInstallation](t, l.enable("gamma", PluginEnablementPutStateEnabled), http.StatusOK)
	require.Equal(t, 2, swapped.Entitlement.EnabledOrganizations)
	require.Nil(t, swapped.PendingOrganizations)
	requireErrorCode(t, l.enable("beta", PluginEnablementPutStateEnabled), http.StatusConflict, "plugin_organization_limit")
	// Changing an enabled organization's settings enables nobody new.
	zone := "Europe/Paris"
	decode[PluginInstallation](t, l.api.call(pluginOperator, http.MethodPut, l.path+"/organizations/acme", uuid.NewString(),
		PluginEnablementPut{ExpectedRevision: l.api.revision(), Reason: "Move acme", State: PluginEnablementPutStateEnabled, Timezone: &zone}), http.StatusOK)

	// A renewal with a higher sequence replaces the grant and its limit.
	l.importGrants("Raise the limit", l.signInstallationGrant(t, l.id, uuid.New(), 2, l.base+10, l.base+20, 3))
	raised := decode[PluginInstallation](t, l.enable("beta", PluginEnablementPutStateEnabled), http.StatusOK)
	require.Equal(t, 3, raised.Entitlement.EnabledOrganizations)
	require.Equal(t, int64(2), raised.Entitlement.InstallationGrant.Sequence)
	require.Equal(t, int64(3), *raised.Entitlement.InstallationGrant.OrganizationLimit)

	// Room for one more: of two organizations created at the same time,
	// exactly one is enabled, because creations take turns on the Plugin
	// Manager lock.
	l.importGrants("Room for one more", l.signInstallationGrant(t, l.id, uuid.New(), 3, l.base+10, l.base+20, 4))
	codes := make(chan int, 2)
	for _, org := range []string{"delta", "epsilon"} {
		go func() {
			codes <- l.api.call(userid.InternalSystemUuid, http.MethodPost, "/internal/orgs", "", InternalOrganizationCreateBody{Id: &org}).Code
		}()
	}
	require.Equal(t, []int{http.StatusCreated, http.StatusCreated}, []int{<-codes, <-codes})
	raced := l.read()
	require.Equal(t, 4, raced.Entitlement.EnabledOrganizations)
	require.Len(t, *raced.PendingOrganizations, 1)
	require.Contains(t, []string{"delta", "epsilon"}, (*raced.PendingOrganizations)[0])
	for _, org := range []string{"acme", "beta", "gamma"} {
		require.NoError(t, l.effect(org), "every enabled organization is covered: %s", org)
	}
	l.importGrants("Lower the limit without withdrawing access", l.signInstallationGrant(t, l.id, uuid.New(), 4, l.base+10, l.base+20, 1))
	require.Equal(t, 4, l.read().Entitlement.EnabledOrganizations)
	for _, org := range []string{"acme", "beta", "gamma"} {
		require.NoError(t, l.effect(org), "a lower limit never disables existing access")
	}
}

func TestRevokedOrganizationGrantFallsBackAndRenewalTakesPrecedence(t *testing.T) {
	l := newLicensedOrganizations(t)
	wide, own := uuid.New(), uuid.New()
	l.importGrants("Import both licence scopes",
		l.signInstallationGrant(t, l.id, wide, 1, l.base+100, l.base+200, 0),
		l.signGrantFor(t, l.id, own, 1, l.base+10, l.base+20))
	require.Equal(t, own, l.grantOf(l.read(), "acme").IssuanceId)
	state, scope := l.served("acme")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeOrganization), scope)
	_, err := model.ImportPluginEntitlementRevocations(t.Context(), l.db, nil, l.signRevocation(t, own), "internal_system", sha256.Sum256([]byte("operator-file")))
	require.NoError(t, err)
	require.Equal(t, wide, l.grantOf(l.read(), "acme").IssuanceId)
	state, scope = l.served("acme")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeInstallation), scope)
	require.NoError(t, l.effect("acme"))
	preflight := decode[PluginCapability](t, l.api.call(uuid.MustParse(l.item.ServiceUserID), http.MethodGet, "/orgs/acme/plugins/"+l.item.ID+"/entitlement", "", nil), http.StatusOK)
	require.Equal(t, grantScope(PluginGrantScopeInstallation), preflight.Entitlement.GrantScope)
	decode[PluginInstallation](t, l.enable("acme", PluginEnablementPutStateDisabled), http.StatusOK)
	require.Error(t, l.effect("acme"), "fallback cannot enable a disabled organization")
	decode[PluginInstallation](t, l.enable("acme", PluginEnablementPutStateEnabled), http.StatusOK)
	renewed := uuid.New()
	l.importGrants("Renew the organization's licence", l.signGrantFor(t, l.id, renewed, 2, l.base+10, l.base+20))
	require.Equal(t, renewed, l.grantOf(l.read(), "acme").IssuanceId)
	state, scope = l.served("acme")
	require.Equal(t, PluginAvailabilityStateAvailable, state)
	require.Equal(t, grantScope(PluginGrantScopeOrganization), scope)
	require.NoError(t, l.effect("acme"))
}
