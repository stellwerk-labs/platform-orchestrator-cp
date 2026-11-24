package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stretchr/testify/require"
)

const nativeCatalogueFixture = `{"contract_version":"stellwerk.runner.catalogue/v1","runtime_version":"stellwerk.runner.rollout/v1","tools_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tools":[{"id":"validate-manifest","name":"Validate manifest","phase":"pre_plan","retry":"read_only","required_capabilities":["local.exec"]}]}`

func TestRunnerNativeExecutionTypedUpdatesPreserveAndClear(t *testing.T) {
	for _, runnerType := range []string{"kubernetes", "kubernetes-gke", "kubernetes-eks", "kubernetes-agent", "serverless-ecs"} {
		t.Run(runnerType, func(t *testing.T) {
			var update RunnerConfigurationUpdate
			body := `{"type":"` + runnerType + `","supported_protocols":["stellwerk.pipeline/v1"],"capabilities":["local.exec"],"native_execution":` + nativeCatalogueFixture + `}`
			require.NoError(t, json.Unmarshal([]byte(body), &update))
			_, parsed, err := checkUpdateBodyRunnerConfigurationStrict(update)
			require.NoError(t, err)
			configuration := updateRunnerConfiguration(map[string]any{"type": runnerType}, parsed)
			require.NoError(t, validateRunnerNativeExecution(configuration))
			public, err := json.Marshal(apiRunnerFromDbRunner(model.Runner{RunnerType: runnerType, RunnerConfiguration: configuration}))
			require.NoError(t, err)
			require.Contains(t, string(public), `"native_execution"`)
			summary, err := json.Marshal(apiRunnerSummaryFromDbRunner(model.Runner{RunnerType: runnerType, RunnerConfiguration: configuration}))
			require.NoError(t, err)
			require.NotContains(t, string(summary), `"native_execution"`)

			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`"}`), &update))
			_, parsed, err = checkUpdateBodyRunnerConfigurationStrict(update)
			require.NoError(t, err)
			unchanged := updateRunnerConfiguration(configuration, parsed)
			require.NoError(t, applyRunnerConfigurationClears(unchanged, &update))
			require.Equal(t, configuration, unchanged)
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","native_execution":null}`), &update))
			_, parsed, err = checkUpdateBodyRunnerConfigurationStrict(update)
			require.NoError(t, err)
			cleared := updateRunnerConfiguration(configuration, parsed)
			require.NoError(t, applyRunnerConfigurationClears(cleared, &update))
			require.NotContains(t, cleared, "native_execution")
			require.Contains(t, configuration, "native_execution")
			var create RunnerConfiguration
			require.NoError(t, json.Unmarshal([]byte(`{"type":"`+runnerType+`","native_execution":null}`), &create))
			_, _, err = checkCreateBodyRunnerConfigurationStrict(create)
			require.ErrorContains(t, err, "cannot be null")
		})
	}
}

func TestRunnerNativeExecutionRejectsInvalidInventory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any, map[string]any)
	}{
		{"unknown_manifest_field", func(c, _ map[string]any) { c["environment"] = map[string]any{"API_KEY": "private-canary"} }},
		{"unknown_tool_field", func(_, tool map[string]any) { tool["executable"] = "/private/canary" }},
		{"unsupported_contract", func(c, _ map[string]any) { c["contract_version"] = "future" }},
		{"unsupported_runtime", func(c, _ map[string]any) { c["runtime_version"] = "future" }},
		{"bad_digest", func(c, _ map[string]any) { c["tools_digest"] = strings.Repeat("A", 64) }},
		{"missing_tools", func(c, _ map[string]any) { delete(c, "tools") }},
		{"too_many_tools", func(c, tool map[string]any) {
			c["tools"] = make([]map[string]any, 33)
			c["tools"].([]map[string]any)[0] = tool
		}},
		{"duplicate_ids", func(c, tool map[string]any) { c["tools"] = []map[string]any{tool, tool} }},
		{"invalid_id", func(_, tool map[string]any) { tool["id"] = "../tool" }},
		{"long_id", func(_, tool map[string]any) { tool["id"] = strings.Repeat("a", 129) }},
		{"blank_name", func(_, tool map[string]any) { tool["name"] = " \t" }},
		{"long_name", func(_, tool map[string]any) { tool["name"] = strings.Repeat("界", 121) }},
		{"control_name", func(_, tool map[string]any) { tool["name"] = "Name\nspoof" }},
		{"unknown_phase", func(_, tool map[string]any) { tool["phase"] = "after" }},
		{"unknown_retry", func(_, tool map[string]any) { tool["retry"] = "repeat" }},
		{"mutating_pre_plan", func(_, tool map[string]any) { tool["retry"] = "receipt" }},
		{"missing_requirements", func(_, tool map[string]any) { delete(tool, "required_capabilities") }},
		{"duplicate_requirements", func(_, tool map[string]any) { tool["required_capabilities"] = []string{"local.exec", "local.exec"} }},
		{"invalid_requirement", func(_, tool map[string]any) { tool["required_capabilities"] = []string{"API_KEY=private-canary"} }},
		{"undeclared_requirement", func(_, tool map[string]any) { tool["required_capabilities"] = []string{"network.remote"} }},
		{"oversized_manifest", func(_, tool map[string]any) { tool["name"] = strings.Repeat("a", 64<<10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var catalogue map[string]any
			require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &catalogue))
			tool := catalogue["tools"].([]any)[0].(map[string]any)
			tc.change(catalogue, tool)
			err := validateRunnerNativeExecution(map[string]any{"native_execution": catalogue, "supported_protocols": []string{"stellwerk.pipeline/v1"}, "capabilities": []string{"local.exec"}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-canary")
		})
	}
}

func TestRunnerNativeExecutionCrossFieldConsistency(t *testing.T) {
	var catalogue map[string]any
	require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &catalogue))
	configuration := map[string]any{"native_execution": catalogue, "supported_protocols": []string{"stellwerk.pipeline/v1"}, "capabilities": []string{"local.exec"}}
	require.NoError(t, validateRunnerNativeExecution(configuration))
	delete(configuration, "capabilities")
	require.ErrorContains(t, validateRunnerNativeExecution(configuration), "undeclared capability")
	configuration["capabilities"] = []string{"local.exec"}
	delete(configuration, "supported_protocols")
	require.ErrorContains(t, validateRunnerNativeExecution(configuration), "native pipeline protocol")
	delete(configuration, "native_execution")
	require.NoError(t, validateRunnerNativeExecution(configuration), "legacy Runners remain valid")
}

func TestNativeAgentPatchRefusesRawKeyAndCapacityNull(t *testing.T) {
	for _, field := range []string{"key", "max_parallel_jobs"} {
		t.Run(field, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{"type": "native-agent", field: nil})
			require.NoError(t, err)
			_, _, err = checkUpdateBodyRunnerConfigurationStrict(RunnerConfigurationUpdate{union: encoded})
			require.ErrorContains(t, err, "cannot be cleared")
		})
	}
}
