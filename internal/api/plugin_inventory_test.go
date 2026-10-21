package api

import (
	"encoding/json"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func TestPluginInventoryProjectionNeverIncludesModuleDefinitions(t *testing.T) {
	snapshot := &model.ExternalInventory{Versions: []model.CoreModuleVersion{{ModuleSlug: "example", Definition: model.ModuleDefinitionVersion{ModuleSourceCode: opt.Of("source-must-not-cross"), ModuleInputs: map[string]any{"credential": "input-must-not-cross"}}}}}
	encoded, err := json.Marshal(externalInventoryToAPI(snapshot))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "source-must-not-cross")
	require.NotContains(t, string(encoded), "input-must-not-cross")
	require.NotContains(t, string(encoded), "module_inputs")
	require.NotContains(t, string(encoded), "module_source_code")
	for _, name := range []string{"environments", "environment_types", "modules", "pins", "pin_events"} {
		require.Contains(t, string(encoded), `"`+name+`":[]`)
	}
}
