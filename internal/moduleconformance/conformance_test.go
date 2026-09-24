package moduleconformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const contractFixture = `{
  "type": "object",
  "required": ["module_inputs", "module_params", "provider_mapping", "dependencies", "coprovisioned", "output_schema"],
  "properties": {
    "module_inputs": {"type":"object", "required":["namespace"], "properties":{"namespace":{"type":"string", "minLength":1}}},
    "module_params": {"type":"object", "additionalProperties":false, "required":["replicas"], "properties":{
      "replicas":{"type":"object", "required":["type","is_optional"], "properties":{"type":{"enum":["number"]},"is_optional":{"enum":[false]}}}
    }},
    "provider_mapping": {"type":"object", "required":["kubernetes"], "properties":{"kubernetes":{"type":"string", "pattern":"^kubernetes[.]"}}},
    "dependencies": {"type":"object", "required":["database"], "properties":{
      "database":{"type":"object", "required":["type"], "properties":{"type":{"enum":["postgres"]}}}
    }},
    "coprovisioned": {"type":"array", "items":{"type":"object", "required":["type","is_dependent_on_current"], "properties":{
      "type":{"type":"string"}, "is_dependent_on_current":{"enum":[true]}
    }}, "not":{"items":{"not":{"required":["type"],"properties":{"type":{"enum":["monitor"]}}}}}},
    "output_schema": {"type":"object", "required":["type"], "properties":{"type":{"enum":["object"]}}}
  }
}`

const definitionFixture = `{
  "module_inputs":{"namespace":"${context.env_id}"},
  "module_params":{"replicas":{"type":"number","is_optional":false}},
  "provider_mapping":{"kubernetes":"kubernetes.cluster"},
  "dependencies":{"database":{"type":"postgres","params":{}}},
  "coprovisioned":[{"type":"monitor","is_dependent_on_current":true,"copy_dependents_from_current":false}],
  "output_schema":{"type":"object"}
}`

func object(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &value))
	return value
}

func requireFailure(t *testing.T, err error, code, path, rule string) {
	t.Helper()
	var failure *Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, code, failure.Code)
	require.Equal(t, path, failure.Path)
	require.Equal(t, rule, failure.Rule)
}

func TestDefinitionConformanceAllDimensions(t *testing.T) {
	contract := object(t, contractFixture)
	require.NoError(t, ValidateContract(t.Context(), contract))
	require.NoError(t, ValidateDefinition(t.Context(), contract, object(t, definitionFixture)))
	for _, tc := range []struct {
		field, value, path, rule string
	}{
		{"module_inputs", `{}`, "/module_inputs/namespace", "required"},
		{"module_inputs", `{"namespace":27}`, "/module_inputs/namespace", "type"},
		{"module_params", `{"replicas":{"type":"string","is_optional":false}}`, "/module_params/replicas/type", "enum"},
		{"module_params", `{"replicas":{"type":"number","is_optional":true}}`, "/module_params/replicas/is_optional", "enum"},
		{"provider_mapping", `{"kubernetes":"aws.cluster"}`, "/provider_mapping/kubernetes", "pattern"},
		{"dependencies", `{"database":{"type":"redis"}}`, "/dependencies/database/type", "enum"},
		{"coprovisioned", `[]`, "/coprovisioned", "not"},
		{"coprovisioned", `[{"type":"monitor","is_dependent_on_current":false}]`, "/coprovisioned/0/is_dependent_on_current", "enum"},
		{"output_schema", `{"type":"string"}`, "/output_schema/type", "enum"},
	} {
		t.Run(tc.path+tc.value, func(t *testing.T) {
			definition := object(t, definitionFixture)
			var value any
			require.NoError(t, json.Unmarshal([]byte(tc.value), &value))
			definition[tc.field] = value
			requireFailure(t, ValidateDefinition(t.Context(), contract, definition), CodeNonconformant, tc.path, tc.rule)
		})
	}
}

func TestRequiredCoprovisionedTypesAcrossMixedArray(t *testing.T) {
	contract := object(t, `{"type":"object", "properties":{"coprovisioned":{"type":"array","items":{"type":"object"},"allOf":[
      {"not":{"items":{"not":{"required":["type"],"properties":{"type":{"enum":["monitor"]}}}}}},
      {"not":{"items":{"not":{"required":["type"],"properties":{"type":{"enum":["backup"]}}}}}}
    ]}}}`)
	require.NoError(t, ValidateContract(t.Context(), contract))
	for _, tc := range []struct {
		values string
		valid  bool
	}{
		{`[{"type":"monitor"},{"type":"unrelated"},{"type":"backup"}]`, true},
		{`[{"type":"backup"},{"type":"monitor"}]`, true},
		{`[{"type":"monitor"},{"type":"monitor"}]`, false},
		{`[{"type":"unrelated"}]`, false},
		{`[{}]`, false},
		{`[]`, false},
	} {
		t.Run(tc.values, func(t *testing.T) {
			err := ValidateDefinition(t.Context(), contract, object(t, `{"coprovisioned":`+tc.values+`}`))
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPlaceholderDeclarationsAreNotEvaluatedOrExempted(t *testing.T) {
	contract := object(t, `{"type":"object","properties":{"module_inputs":{"type":"object","properties":{"replicas":{"type":"number"}}}}}`)
	require.NoError(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"replicas":3}}`)))
	requireFailure(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"replicas":"${resources.capacity.outputs.count}"}}`)),
		CodeNonconformant, "/module_inputs/replicas", "type")
	contract = object(t, `{"type":"object","properties":{"module_inputs":{"type":"object","properties":{"replicas":{"type":"string","pattern":"^[$][{].+[}]$"}}}}}`)
	require.NoError(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"replicas":"${resources.capacity.outputs.count}"}}`)))
}

func TestSupportedSchemaValueRules(t *testing.T) {
	for _, tc := range []struct {
		name, schema, accepted, rejected, pathSuffix string
	}{
		{"number range and multiple", `{"type":"number","minimum":0,"exclusiveMinimum":true,"maximum":10,"exclusiveMaximum":true,"multipleOf":2}`, `4`, `3`, ""},
		{"exclusive minimum", `{"type":"number","minimum":0,"exclusiveMinimum":true}`, `1`, `0`, ""},
		{"exclusive maximum", `{"type":"number","maximum":10,"exclusiveMaximum":true}`, `9`, `10`, ""},
		{"integer", `{"type":"integer"}`, `2`, `2.5`, ""},
		{"string bounds", `{"type":"string","minLength":2,"maxLength":3}`, `"ab"`, `"a"`, ""},
		{"string maximum", `{"type":"string","maxLength":3}`, `"abc"`, `"abcd"`, ""},
		{"array bounds and uniqueness", `{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":2,"uniqueItems":true}`, `["a","b"]`, `["a","a"]`, ""},
		{"array minimum", `{"type":"array","items":{},"minItems":1}`, `[1]`, `[]`, ""},
		{"array maximum", `{"type":"array","items":{},"maxItems":1}`, `[1]`, `[1,2]`, ""},
		{"additional property schema", `{"type":"object","additionalProperties":{"type":"boolean"},"minProperties":1,"maxProperties":2}`, `{"enabled":true}`, `{"enabled":"true"}`, "/enabled"},
		{"property minimum", `{"type":"object","minProperties":1}`, `{"a":true}`, `{}`, ""},
		{"property maximum", `{"type":"object","maxProperties":1}`, `{"a":true}`, `{"a":true,"b":false}`, ""},
		{"closed object", `{"type":"object","properties":{"a":{}},"additionalProperties":false}`, `{"a":true}`, `{"b":true}`, ""},
		{"nullable", `{"type":"boolean","nullable":true}`, `null`, `"true"`, ""},
		{"nonnullable", `{"type":"boolean","nullable":false}`, `false`, `null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := object(t, `{"type":"object","properties":{"module_inputs":{"properties":{"value":`+tc.schema+`}}}}`)
			require.NoError(t, ValidateContract(t.Context(), contract))
			require.NoError(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"value":`+tc.accepted+`}}`)))
			var failure *Error
			require.ErrorAs(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"value":`+tc.rejected+`}}`)), &failure)
			require.Equal(t, CodeNonconformant, failure.Code)
			require.Equal(t, "/module_inputs/value"+tc.pathSuffix, failure.Path)
		})
	}
}

func TestInvalidContractsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, contract, path, rule string
	}{
		{"empty", `{}`, "/module_contract/type", "type"},
		{"wrong root", `{"type":"array","items":{}}`, "/module_contract/type", "type"},
		{"local ref", `{"type":"object","properties":{"module_inputs":{"$ref":"#/secret"}}}`, "/module_contract/properties/module_inputs/$ref", "unsupported_keyword"},
		{"external ref", `{"type":"object","$ref":"https://example.invalid/secret"}`, "/module_contract/$ref", "unsupported_keyword"},
		{"type array", `{"type":"object","properties":{"module_inputs":{"type":["object","null"]}}}`, "/module_contract/properties/module_inputs/type", "type"},
		{"array needs items", `{"type":"object","properties":{"coprovisioned":{"type":"array"}}}`, "/module_contract/properties/coprovisioned/items", "required"},
		{"tuple items", `{"type":"object","properties":{"coprovisioned":{"items":[{}]}}}`, "/module_contract/properties/coprovisioned/items", "schema_object"},
		{"root source", `{"type":"object","properties":{"module_source":{}}}`, "/module_contract/properties/module_source", "definition_field"},
		{"composition root source", `{"type":"object","allOf":[{"properties":{"module_source":{}}}]}`, "/module_contract/allOf/0/properties/module_source", "definition_field"},
		{"not root lifecycle", `{"type":"object","not":{"required":["lifecycle_status"]}}`, "/module_contract/not/required/0", "definition_field"},
		{"invalid regex", `{"type":"object","properties":{"module_inputs":{"pattern":"[SECRET"}}}`, "/module_contract/properties/module_inputs/pattern", "pattern"},
		{"negative count", `{"type":"object","maxProperties":-1}`, "/module_contract/maxProperties", "nonnegative_integer"},
		{"fractional count", `{"type":"object","minProperties":1.5}`, "/module_contract/minProperties", "nonnegative_integer"},
		{"zero multiple", `{"type":"object","multipleOf":0}`, "/module_contract/multipleOf", "positive"},
		{"required type", `{"type":"object","required":[42]}`, "/module_contract/required/0", "type"},
		{"duplicate required", `{"type":"object","required":["module_inputs","module_inputs"]}`, "/module_contract/required", "uniqueItems"},
		{"empty enum", `{"type":"object","enum":[]}`, "/module_contract/enum", "nonempty_array"},
		{"empty allOf", `{"type":"object","allOf":[]}`, "/module_contract/allOf", "schema_array"},
		{"nonboolean nullable", `{"type":"object","nullable":"true"}`, "/module_contract/nullable", "type"},
		{"null additionalProperties", `{"type":"object","additionalProperties":null}`, "/module_contract/additionalProperties", "schema_object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireFailure(t, ValidateContract(t.Context(), object(t, tc.contract)), CodeInvalidContract, tc.path, tc.rule)
		})
	}
	for _, keyword := range []string{"default", "example", "format", "anyOf", "oneOf", "contains", "$id", "$defs", "patternProperties", "x-executable"} {
		t.Run(keyword, func(t *testing.T) {
			contract := map[string]any{"type": "object", keyword: "SECRET"}
			requireFailure(t, ValidateContract(t.Context(), contract), CodeInvalidContract, "/module_contract/"+keyword, "unsupported_keyword")
		})
	}
}

func TestContractComplexityBoundaries(t *testing.T) {
	contract := map[string]any{"type": "object", "description": ""}
	raw, err := json.Marshal(contract)
	require.NoError(t, err)
	contract["description"] = strings.Repeat("a", MaxContractBytes-len(raw))
	require.NoError(t, ValidateContract(t.Context(), contract))
	contract["description"] = contract["description"].(string) + "a"
	requireFailure(t, ValidateContract(t.Context(), contract), CodeInvalidContract, "/module_contract", "maxBytes")

	for _, tc := range []struct{ nodes, depth int }{{MaxContractNodes, 2}, {MaxContractNodes + 1, 2}, {16, MaxContractDepth}, {17, MaxContractDepth + 1}} {
		t.Run(fmt.Sprintf("%d nodes %d depth", tc.nodes, tc.depth), func(t *testing.T) {
			contract := map[string]any{"type": "object"}
			if tc.depth == 2 {
				branches := make([]any, tc.nodes-1)
				for i := range branches {
					branches[i] = map[string]any{}
				}
				contract["allOf"] = branches
			} else {
				current := contract
				for i := 1; i < tc.depth; i++ {
					child := map[string]any{}
					current["not"] = child
					current = child
				}
			}
			err := ValidateContract(t.Context(), contract)
			if tc.nodes <= MaxContractNodes && tc.depth <= MaxContractDepth {
				require.NoError(t, err)
			} else {
				var failure *Error
				require.ErrorAs(t, err, &failure)
				require.Equal(t, CodeInvalidContract, failure.Code)
				require.Contains(t, []string{"maxNodes", "maxDepth"}, failure.Rule)
			}
		})
	}
}

func TestDeterministicRedactedErrorsAndNoMutation(t *testing.T) {
	contract := object(t, `{"type":"object","properties":{"module_inputs":{"type":"object","required":["z-missing","a-missing"],"properties":{"password":{"enum":["EXPECTED_SECRET"]}}}}}`)
	definition := object(t, `{"module_inputs":{"password":"ACTUAL_SECRET"}}`)
	beforeContract, _ := json.Marshal(contract)
	beforeDefinition, _ := json.Marshal(definition)
	for range 20 {
		err := ValidateDefinition(t.Context(), contract, definition)
		requireFailure(t, err, CodeNonconformant, "/module_inputs/a-missing", "required")
		require.NotContains(t, err.Error(), "SECRET")
		require.NotContains(t, err.Error(), "Schema:")
		require.NotContains(t, err.Error(), "Value:")
	}
	afterContract, _ := json.Marshal(contract)
	afterDefinition, _ := json.Marshal(definition)
	require.Equal(t, string(beforeContract), string(afterContract))
	require.Equal(t, string(beforeDefinition), string(afterDefinition))

	contract = object(t, `{"type":"object","properties":{"module_inputs":{"properties":{"a/b~c":{"type":"number"}}}}}`)
	requireFailure(t, ValidateDefinition(t.Context(), contract, object(t, `{"module_inputs":{"a/b~c":"SECRET"}}`)),
		CodeNonconformant, "/module_inputs/a~1b~0c", "type")
	// A keyword-looking property name or enum value is data, not a schema ref.
	require.NoError(t, ValidateContract(t.Context(), object(t, `{"type":"object","properties":{"output_schema":{"properties":{"$ref":{"enum":[{"$ref":"https://example.invalid"}]}}}}}`)))

	contract = object(t, `{"type":"object","properties":{"module_inputs":{"properties":{"password":{"enum":["EXPECTED_SECRET"]}}}}}`)
	err := ValidateDefinition(t.Context(), contract, definition)
	requireFailure(t, err, CodeNonconformant, "/module_inputs/password", "enum")
	require.NotContains(t, fmt.Sprintf("%v %#v", err, err), "SECRET")
	err = ValidateContract(t.Context(), object(t, `{"type":"object","properties":{"module_inputs":{"pattern":"[SECRET"}}}`))
	require.Error(t, err)
	require.NotContains(t, fmt.Sprintf("%v %#v", err, err), "SECRET")
}

func TestLegacyContractAbsenceAndInvalidJSON(t *testing.T) {
	require.NoError(t, ValidateContract(t.Context(), nil))
	require.NoError(t, ValidateDefinition(t.Context(), nil, object(t, definitionFixture)))
	require.NoError(t, ValidateDefinition(t.Context(), map[string]any{"type": "object"}, object(t, definitionFixture)))
	invalidValue := map[string]any{"type": "object", "description": make(chan int)}
	requireFailure(t, ValidateContract(t.Context(), invalidValue), CodeInvalidContract, "/module_contract", "json")
	requireFailure(t, ValidateDefinition(t.Context(), map[string]any{"type": "object"}, map[string]any{"module_inputs": make(chan int)}), CodeNonconformant, "", "json")
}

func TestOutputDeclarationRequirementAndJSONEquality(t *testing.T) {
	required := object(t, `{"type":"object","properties":{"port":{"type":"integer","minimum":1}},"required":["port"]}`)
	require.NoError(t, ValidateOutputs(nil, nil))
	require.NoError(t, ValidateOutputs(map[string]any{}, nil))
	require.NoError(t, ValidateOutputs(map[string]any{}, required))
	requireFailure(t, ValidateOutputs(required, nil), CodeOutputRequired, "/output_schema", "required")
	requireFailure(t, ValidateOutputs(required, map[string]any{}), CodeOutputMismatch, "/output_schema", "equality")
	require.NoError(t, ValidateOutputs(required, object(t, `{"required":["port"],"properties":{"port":{"minimum":1.0,"type":"integer"}},"type":"object"}`)))
	requireFailure(t, ValidateOutputs(required, object(t, `{"type":"object"}`)), CodeOutputMismatch, "/output_schema", "equality")
	requireFailure(t, ValidateOutputs(map[string]any{"required": []any{"a", "b"}}, map[string]any{"required": []any{"b", "a"}}), CodeOutputMismatch, "/output_schema", "equality")
	requireFailure(t, ValidateOutputs(map[string]any{"nullable": true}, map[string]any{"nullable": "true"}), CodeOutputMismatch, "/output_schema", "equality")
	requireFailure(t, ValidateOutputs(map[string]any{"description": nil}, map[string]any{"other": nil}), CodeOutputMismatch, "/output_schema", "equality")
	require.NoError(t, ValidateOutputs(map[string]any{"enum": []any{json.Number("1e3")}}, map[string]any{"enum": []any{1000}}))
	requireFailure(t, ValidateOutputs(map[string]any{"minimum": int64(9007199254740992)}, map[string]any{"minimum": int64(9007199254740993)}), CodeOutputMismatch, "/output_schema", "equality")
	requireFailure(t, ValidateOutputs(required, map[string]any{"secret": make(chan int)}), CodeOutputMismatch, "/output_schema", "json")
}
