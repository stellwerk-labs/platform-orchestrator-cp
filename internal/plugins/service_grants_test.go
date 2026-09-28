package plugins

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestInstallationServiceGrantsAreExplicitAndNamespaced(t *testing.T) {
	item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test", Organizations: map[string]AvailabilityState{"acme": StateAvailable}, ServiceUserID: uuid.NewString(), AdmissionCheckerID: uuid.NewString(), ServiceGrants: []ServiceGrant{{Action: "external-deployment.admit", Permission: "plugin.dev.example.workflow.control", Scope: "environment"}}, Operations: []Operation{{ID: "prepare", Method: "POST", Path: "/orgs/{orgId}/workflows/{id}/prepare", Mutating: true, ServiceActions: []string{"external-deployment.admit"}}}}
	load := func(t *testing.T, value Installation) error {
		t.Helper()
		encoded, err := json.Marshal([]Installation{value})
		require.NoError(t, err)
		_, err = LoadInstallations(strings.NewReader(string(encoded)), false)
		return err
	}
	require.NoError(t, load(t, item))
	for _, permission := range []string{"module.version.unpin", "plugin.dev.other.workflow.control", "plugin.dev.example.workflow.nested.control"} {
		t.Run(permission, func(t *testing.T) {
			changed := item
			changed.ServiceGrants = []ServiceGrant{{Action: "external-deployment.admit", Permission: permission, Scope: "environment"}}
			require.Error(t, load(t, changed))
		})
	}
	for _, identity := range []string{"", "not-a-uuid", uuid.Nil.String(), "ffffffff-ffff-ffff-ffff-ffffffffffff", item.AdmissionCheckerID} {
		changed := item
		changed.ServiceUserID = identity
		require.Error(t, load(t, changed))
	}
	changed := item
	changed.ServiceGrants = nil
	require.Error(t, load(t, changed))
	changed = item
	changed.Operations = []Operation{{ID: "prepare", Method: "POST", Path: "/prepare", Mutating: true, ServiceActions: []string{"module.version.unpin"}}}
	require.Error(t, load(t, changed))
	changed = item
	changed.Operations = []Operation{{ID: "core.listModules", Method: "GET", Path: "/steal-core-operation", Mutating: false}}
	require.Error(t, load(t, changed), "reserved host IDs must never route to a plugin backend")
	changed = item
	changed.UI = &UIContribution{SDKVersion: "1", Origin: "https://ui.example.test", Entrypoint: "/index.html", RoutePrefix: "/workflow", NavigationLabel: "Workflow", CoreOperations: []HostOperation{{ID: "core.listModules", Method: "GET"}}}
	require.NoError(t, load(t, changed))
	changed.UI.CoreOperations[0].ID = "listWorkflows"
	require.Error(t, load(t, changed), "plugin actions must not acquire host provenance")
}

func TestInstallationAuthorizationPoliciesAreClosedAndNamespaced(t *testing.T) {
	const pluginID = "dev.example.workflow"
	permission := "plugin." + pluginID + ".rollout-read"
	item := Installation{
		ID: pluginID, DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test",
		Organizations: map[string]AvailabilityState{"acme": StateAvailable}, ServiceUserID: uuid.NewString(), AdmissionCheckerID: uuid.NewString(),
		ServiceGrants:         []ServiceGrant{{Action: "authorization.check", Permission: permission, Scope: "organization"}},
		AuthorizationPolicies: []AuthorizationPolicy{{Permission: permission, Scopes: []string{"organization", "project", "environment"}}},
		Operations:            []Operation{{ID: "read", Method: "GET", Path: "/orgs/{orgId}/workflows", ServiceActions: []string{"authorization.check"}}},
	}
	load := func(t *testing.T, value Installation) error {
		t.Helper()
		encoded, err := json.Marshal([]Installation{value})
		require.NoError(t, err)
		_, err = LoadInstallations(strings.NewReader(string(encoded)), false)
		return err
	}
	require.NoError(t, load(t, item))

	for name, change := range map[string]func(*Installation){
		"core permission": func(changed *Installation) { changed.AuthorizationPolicies[0].Permission = "module.version.unpin" },
		"other plugin": func(changed *Installation) {
			changed.AuthorizationPolicies[0].Permission = "plugin.dev.other.workflow.rollout-read"
		},
		"nested local capability": func(changed *Installation) {
			changed.AuthorizationPolicies[0].Permission = "plugin." + pluginID + ".rollout.read"
		},
		"unknown scope": func(changed *Installation) { changed.AuthorizationPolicies[0].Scopes = []string{"rollout"} },
		"duplicate scope": func(changed *Installation) {
			changed.AuthorizationPolicies[0].Scopes = []string{"organization", "organization"}
		},
		"missing policy":              func(changed *Installation) { changed.AuthorizationPolicies = nil },
		"missing check grant":         func(changed *Installation) { changed.ServiceGrants[0].Action = "inventory.read" },
		"uncovered check grant scope": func(changed *Installation) { changed.AuthorizationPolicies[0].Scopes = []string{"environment"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := item
			changed.ServiceGrants = append([]ServiceGrant(nil), item.ServiceGrants...)
			changed.AuthorizationPolicies = []AuthorizationPolicy{{Permission: item.AuthorizationPolicies[0].Permission, Scopes: append([]string(nil), item.AuthorizationPolicies[0].Scopes...)}}
			change(&changed)
			require.Error(t, load(t, changed))
		})
	}
}
