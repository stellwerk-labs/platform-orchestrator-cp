package plugins

import (
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/pluginstatus"
	"github.com/stretchr/testify/require"
)

func TestManifestKeepsTheStatusContractToCore(t *testing.T) {
	manifest := Manifest{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", MigrationGeneration: 4,
		Operations: []Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}}}
	require.NoError(t, manifest.Validate())

	negative := manifest
	negative.MigrationGeneration = -1
	require.ErrorContains(t, negative.Validate(), "migration generation")

	for _, path := range []string{pluginstatus.Path, "/.well-known/anything"} {
		shadowing := manifest
		shadowing.Operations = []Operation{{ID: "status", Method: "GET", Path: path}}
		require.ErrorContains(t, shadowing.Validate(), "reserved path prefix", path)
	}
	require.True(t, strings.HasPrefix(pluginstatus.Path, reservedPathPrefix), "the status endpoint lies in the reserved prefix")

	decoded, err := DecodeManifest([]byte(`{"id":"dev.example.workflow","display_name":"Workflow","version":"1.0.0","migration_generation":7}`))
	require.NoError(t, err)
	require.EqualValues(t, 7, decoded.MigrationGeneration)
}

func TestManifestContributionsCannotCollide(t *testing.T) {
	workflow := func() Manifest {
		return Manifest{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0",
			Capabilities: []string{"plugin.dev.example.workflow.read", "plugin.dev.example.workflow.approve"},
			UI: &UIContribution{SDKVersion: "1", Entrypoint: "/index.html", RoutePrefix: "/workflow", NavigationLabel: "Workflow",
				CoreOperations: []HostOperation{{ID: "core.listModules", Method: "GET"}}},
			Operations: []Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}, {ID: "start", Method: "POST", Path: "/orgs/{orgId}/items", Mutating: true}}}
	}
	require.NoError(t, workflow().Validate())

	for name, change := range map[string]func(*Manifest){
		"a capability twice":           func(m *Manifest) { m.Capabilities = append(m.Capabilities, "plugin.dev.example.workflow.read") },
		"another plugin's capability":  func(m *Manifest) { m.Capabilities = []string{"plugin.dev.example.other.read"} },
		"a nested plugin's capability": func(m *Manifest) { m.Capabilities = []string{"plugin.dev.example.workflow.nested.read"} },
		"a Core permission":            func(m *Manifest) { m.Capabilities = []string{"module.version.unpin"} },
		"a name outside any namespace": func(m *Manifest) { m.Capabilities = []string{"read"} },
		"an operation twice": func(m *Manifest) {
			m.Operations = append(m.Operations, Operation{ID: "list", Method: "GET", Path: "/orgs/{orgId}/other"})
		},
		"an operation in Core's host namespace": func(m *Manifest) {
			m.Operations = append(m.Operations, Operation{ID: "core.listProjects", Method: "GET", Path: "/orgs/{orgId}/projects"})
		},
		"a Core host operation twice": func(m *Manifest) {
			m.UI.CoreOperations = append(m.UI.CoreOperations, HostOperation{ID: "core.listModules", Method: "GET"})
		},
	} {
		manifest := workflow()
		change(&manifest)
		require.Error(t, manifest.Validate(), name)
	}

	// A UI route prefix has one spelling, so prefixes the Console serves
	// alike are equal and only one installation can register them.
	for _, prefix := range []string{"/workflow/runs", "/work-flow", "/2026/workflow"} {
		manifest := workflow()
		manifest.UI.RoutePrefix = prefix
		require.NoError(t, manifest.Validate(), prefix)
	}
	for _, prefix := range []string{"/workflow/", "workflow", "/Workflow", "/work_flow", "/workflow//runs", "//workflow", "/", "", "/workflow/-", "/workflow/../admin", "/workflow/./runs", "/workflow%2Fadmin"} {
		manifest := workflow()
		manifest.UI.RoutePrefix = prefix
		require.ErrorContains(t, manifest.Validate(), "invalid plugin UI contribution", prefix)
	}
}
