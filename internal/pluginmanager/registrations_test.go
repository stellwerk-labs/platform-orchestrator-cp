package pluginmanager

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginentitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

const testManifest = `{
	"id": "dev.example.rollouts", "display_name": "Rollouts", "version": "1.0.0", "capabilities": [
		{"id": "plugin.dev.example.rollouts.read", "display_name": "Read rollouts", "description": "View rollouts.", "category": "Rollouts", "level": "read", "scopes": ["organization", "environment"]},
		{"id": "plugin.dev.example.rollouts.approve", "display_name": "Approve rollouts", "description": "Approve a frozen scope.", "category": "Rollouts", "level": "write", "scopes": ["environment", "project"]}
	],
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
		model.PluginRegistrationCapability:          {"plugin.dev.example.rollouts.read", "plugin.dev.example.rollouts.approve"},
		model.PluginRegistrationOperation:           {"start", "list"},
		model.PluginRegistrationServiceGrant:        {"resource-events.read", "authorization.check"},
		model.PluginRegistrationAuthorizationPolicy: {"plugin.dev.example.rollouts.approve"},
		model.PluginRegistrationUI:                  {"/rollouts"},
	}, kinds)

	installation := model.PluginInstallation{InstallationID: uuid.New(), PluginID: manifest.ID, PackageSHA256: strings.Repeat("a", 64), State: model.PluginInstallationActive,
		BackendURL: "https://rollouts.example.test", UIOrigin: "https://rollouts-ui.example.test",
		ServiceUserID: userid.NewServiceUserTokenId().String(), AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
		Organizations: []model.PluginEnablement{
			{OrgID: "acme", State: model.PluginEnablementEnabled, Timezone: "Europe/Berlin", ApprovalPolicies: map[string]string{"environment": "plugin.dev.example.rollouts.approve"}},
			{OrgID: "beta", State: model.PluginEnablementDisabled, Timezone: "America/New_York", ApprovalPolicies: map[string]string{"project": "plugin.dev.example.rollouts.approve"}},
			{OrgID: "gamma", State: model.PluginEnablementEnabled, Timezone: "UTC", ApprovalPolicies: map[string]string{}},
		}}
	// The database returns registrations ordered by kind and key.
	slices.SortFunc(rows, func(a, b model.PluginRegistration) int { return strings.Compare(a.Kind+"/"+a.Key, b.Kind+"/"+b.Key) })
	served, err := servedInstallation(model.PluginSnapshotInstallation{Installation: installation, Package: model.PluginPackage{Manifest: raw}, Registrations: rows})
	require.NoError(t, err)
	require.NoError(t, plugins.ValidateInstallation(served, false))
	require.Equal(t, installation.InstallationID.String(), served.InstallationID)
	require.Equal(t, installation.PackageSHA256, served.ManifestSHA256)
	require.Equal(t, "https://rollouts-ui.example.test", served.UI.Origin, "the UI origin is operator data")
	require.Equal(t, map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable, "beta": plugins.StateNotEnabled, "gamma": plugins.StateAvailable}, served.Organizations,
		"one installation serves every organization it is enabled for, each in its own state")
	require.Equal(t, map[string]string{"acme": "Europe/Berlin", "beta": "America/New_York", "gamma": "UTC"}, served.OrganizationTimezones)
	require.Equal(t, map[string]map[string]string{"acme": installation.Organizations[0].ApprovalPolicies, "beta": installation.Organizations[1].ApprovalPolicies},
		served.OrganizationApprovalPolicies, "approval policies stay with their organization")
	served.UI.Origin = ""
	sortedManifest := manifest
	sortedManifest.Capabilities = []plugins.Capability{manifest.Capabilities[1], manifest.Capabilities[0]}
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
		Organizations: []model.PluginEnablement{{OrgID: "acme", State: model.PluginEnablementEnabled}, {OrgID: "beta", State: model.PluginEnablementDisabled}}}
	served, err := servedInstallation(model.PluginSnapshotInstallation{Installation: twoOrganizations, Package: model.PluginPackage{Manifest: raw}, Registrations: rows})
	require.NoError(t, err)
	require.Equal(t, map[string]plugins.AvailabilityState{"acme": plugins.StateAvailable, "beta": plugins.StateNotEnabled}, served.Organizations,
		"disabling one organization leaves the other available")
	_, err = servedInstallation(model.PluginSnapshotInstallation{Installation: twoOrganizations, Package: model.PluginPackage{Manifest: raw}, Registrations: []model.PluginRegistration{{Kind: "hook", Key: "x", Definition: json.RawMessage(`{}`)}}})
	require.Error(t, err, "an unknown registration kind is never served")
}

func TestPluginManagerRequiresItsDependencies(t *testing.T) {
	adapter, _, err := pluginentitlements.New(unusedDatabase{}, "development", "", nil)
	require.NoError(t, err)
	iam, err := orchestratoriam.NewClientWithResponses("http://iam.example.test")
	require.NoError(t, err)
	_, err = New(Config{Database: unusedDatabase{}, Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, StatusClient: http.DefaultClient, IAM: iam})
	require.NoError(t, err)
	_, err = New(Config{Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, StatusClient: http.DefaultClient, IAM: iam})
	require.Error(t, err)
	_, err = New(Config{Database: unusedDatabase{}, Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, IAM: iam})
	require.Error(t, err, "activation cannot read plugin status without a client")
	_, err = New(Config{Database: unusedDatabase{}, Registry: &plugins.InstallationRegistry{}, Entitlements: adapter, StatusClient: http.DefaultClient})
	require.Error(t, err, "capabilities cannot be registered without IAM")
}

// unusedDatabase satisfies the configuration; these tests never reach it.
type unusedDatabase struct{ model.Databaser }
