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
