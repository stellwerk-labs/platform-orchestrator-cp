package pluginmanager

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

const testManifest = `{
	"id": "dev.example.rollouts", "display_name": "Rollouts", "version": "1.0.0", "capabilities": ["rollouts", "approvals"],
	"ui": {"sdk_version": "1", "entrypoint": "/index.html", "route_prefix": "/rollouts", "navigation_label": "Rollouts",
		"contextual_actions": [{"id": "open", "resource_type": "module_version", "label": "Open", "required_operation_id": "start",
			"route": {"path": "rollouts/new", "query": {"module": "$module_id"}}, "when": {"lifecycle_status": ["proposed"], "semantic_version_required": true}}]},
	"operations": [
		{"id": "start", "method": "POST", "path": "/orgs/{orgId}/rollouts", "mutating": true, "service_actions": ["authorization.check"]},
		{"id": "list", "method": "GET", "path": "/orgs/{orgId}/rollouts"}
	],
	"service_grants": [
		{"action": "resource-events.read", "permission": "plugin.dev.example.rollouts.read", "scope": "organization"},
		{"action": "authorization.check", "permission": "plugin.dev.example.rollouts.approve", "scope": "environment"}
	],
	"authorization_policies": [{"permission": "plugin.dev.example.rollouts.approve", "scopes": ["environment", "project"]}]
}`

func testPackage(t *testing.T) (plugins.Manifest, json.RawMessage) {
	t.Helper()
	manifest, err := plugins.DecodeManifest([]byte(testManifest))
	require.NoError(t, err)
	require.NoError(t, manifest.Validate())
	return manifest, json.RawMessage(testManifest)
}

func TestRegistrationsServeExactlyThePackageContributions(t *testing.T) {
	manifest, raw := testPackage(t)
	rows, err := registrations(manifest)
	require.NoError(t, err)
	kinds := map[string][]string{}
	for _, row := range rows {
		kinds[row.Kind] = append(kinds[row.Kind], row.Key)
	}
	require.Equal(t, map[string][]string{
		model.PluginRegistrationCapability:          {"rollouts", "approvals"},
		model.PluginRegistrationOperation:           {"start", "list"},
		model.PluginRegistrationServiceGrant:        {"resource-events.read", "authorization.check"},
		model.PluginRegistrationAuthorizationPolicy: {"plugin.dev.example.rollouts.approve"},
		model.PluginRegistrationUI:                  {"/rollouts"},
	}, kinds)

	installation := model.PluginInstallation{InstallationID: uuid.New(), PluginID: manifest.ID, PackageSHA256: strings.Repeat("a", 64), State: model.PluginInstallationActive,
		BackendURL: "https://rollouts.example.test", UIOrigin: "https://rollouts-ui.example.test",
		ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		Organizations: []model.PluginEnablement{{OrgID: "acme", State: model.PluginEnablementEnabled, Timezone: "Europe/Berlin", ApprovalPolicies: map[string]string{"environment": "plugin.dev.example.rollouts.approve"}}}}
	// The database returns registrations ordered by kind and key.
	slices.SortFunc(rows, func(a, b model.PluginRegistration) int { return strings.Compare(a.Kind+"/"+a.Key, b.Kind+"/"+b.Key) })
	served, err := servedInstallation(model.PluginSnapshotInstallation{Installation: installation, Package: model.PluginPackage{Manifest: raw}, Registrations: rows})
	require.NoError(t, err)
	require.NoError(t, plugins.ValidateInstallation(served, false))
	require.Equal(t, installation.InstallationID.String(), served.InstallationID)
	require.Equal(t, installation.PackageSHA256, served.ManifestSHA256)
	require.Equal(t, "https://rollouts-ui.example.test", served.UI.Origin, "the UI origin is operator data")
	require.Equal(t, map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable}, served.Organizations)
	require.Equal(t, map[string]string{"acme": "Europe/Berlin"}, served.OrganizationTimezones)
	require.Equal(t, installation.Organizations[0].ApprovalPolicies, served.DeploymentApprovalPolicies)
	served.UI.Origin = ""
	sortedManifest := manifest
	sortedManifest.Capabilities = []string{"approvals", "rollouts"}
	sortedManifest.Operations = []plugins.Operation{manifest.Operations[1], manifest.Operations[0]}
	sortedManifest.ServiceGrants = []plugins.ServiceGrant{manifest.ServiceGrants[1], manifest.ServiceGrants[0]}
	require.Equal(t, sortedManifest, served.Manifest(), "registrations round-trip every contribution")
}

func TestAvailabilityIsDerivedFromInstallationAndEnablement(t *testing.T) {
	manifest, raw := testPackage(t)
	rows, err := registrations(manifest)
	require.NoError(t, err)
	for _, test := range []struct {
		installation, enablement string
		want                     plugins.AvailabilityState
	}{
		{model.PluginInstallationActive, model.PluginEnablementEnabled, plugins.StateAvailable},
		{model.PluginInstallationActive, model.PluginEnablementDisabled, plugins.StateNotEnabled},
		{model.PluginInstallationDisabled, model.PluginEnablementEnabled, plugins.StateNotEnabled},
		{model.PluginInstallationDisabled, model.PluginEnablementDisabled, plugins.StateNotEnabled},
	} {
		installation := model.PluginInstallation{InstallationID: uuid.New(), PluginID: manifest.ID, PackageSHA256: strings.Repeat("a", 64), State: test.installation,
			Organizations: []model.PluginEnablement{{OrgID: "acme", State: test.enablement, Timezone: "UTC"}}}
		served, err := servedInstallation(model.PluginSnapshotInstallation{Installation: installation, Package: model.PluginPackage{Manifest: raw}, Registrations: rows})
		require.NoError(t, err)
		require.Equal(t, test.want, served.Organizations["acme"], "%s installation, %s enablement", test.installation, test.enablement)
	}
	twoOrganizations := model.PluginInstallation{InstallationID: uuid.New(), PluginID: manifest.ID, State: model.PluginInstallationActive,
		Organizations: []model.PluginEnablement{{OrgID: "acme"}, {OrgID: "beta"}}}
	_, err = servedInstallation(model.PluginSnapshotInstallation{Installation: twoOrganizations, Package: model.PluginPackage{Manifest: raw}, Registrations: rows})
	require.ErrorContains(t, err, "exactly one organization", "v1alpha never serves a second organization")
	_, err = servedInstallation(model.PluginSnapshotInstallation{Installation: twoOrganizations, Package: model.PluginPackage{Manifest: raw}, Registrations: []model.PluginRegistration{{Kind: "hook", Key: "x", Definition: json.RawMessage(`{}`)}}})
	require.Error(t, err, "an unknown registration kind is never served")
}

func TestOnlyConfiguredOperatorsHoldPluginManagerPermissions(t *testing.T) {
	writer, reader, orgAdmin := userid.NewHumanUserId(), userid.NewHumanUserId(), userid.NewHumanUserId()
	adapter, _, err := pluginentitlements.New(unusedDatabase{}, "development", "", nil)
	require.NoError(t, err)
	manager, err := New(Config{Database: unusedDatabase{}, Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, Readers: []uuid.UUID{reader}, Writers: []uuid.UUID{writer}})
	require.NoError(t, err)
	require.True(t, manager.Authorize(writer, PermissionWrite))
	require.True(t, manager.Authorize(writer, PermissionRead), "writers read")
	require.True(t, manager.Authorize(reader, PermissionRead))
	require.False(t, manager.Authorize(reader, PermissionWrite))
	for name, actor := range map[string]uuid.UUID{
		"organization admin": orgAdmin, "plugin workload": userid.NewServiceUserTokenId(), "internal system": userid.InternalSystemUuid, "nil": uuid.Nil,
	} {
		require.False(t, manager.Authorize(actor, PermissionRead), name)
		require.False(t, manager.Authorize(actor, PermissionWrite), name)
	}
	var disabled *Manager
	require.False(t, disabled.Authorize(writer, PermissionRead), "no Plugin Manager, no holder")

	for name, holder := range map[string]uuid.UUID{"service user": userid.NewServiceUserTokenId(), "internal system": userid.InternalSystemUuid, "nil": uuid.Nil} {
		_, err := New(Config{Database: unusedDatabase{}, Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, Writers: []uuid.UUID{holder}})
		require.Error(t, err, "%s can never hold a Plugin Manager permission", name)
	}
	_, err = New(Config{Registry: &plugins.InstallationRegistry{}, Entitlements: adapter})
	require.Error(t, err)
}

// unusedDatabase satisfies the configuration; these tests never reach it.
type unusedDatabase struct{ model.Databaser }
