// Command generate-pipeline-schema projects the authoring OpenAPI schema for editors.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

func main() {
	raw, err := os.ReadFile("openapi/pipelines.yaml")
	must(err)
	var spec map[string]any
	must(yaml.Unmarshal(raw, &spec))
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	result := pipelineDocumentSchema(schemas)
	data, err := json.MarshalIndent(result, "", "  ")
	must(err)
	must(os.WriteFile("internal/api/pipeline-document.schema.json", append(data, '\n'), 0600))
}

func pipelineDocumentSchema(schemas map[string]any) map[string]any {
	definitions := map[string]any{}
	result := projectSchema(schemas["PipelineDraftCreate"], schemas, definitions).(map[string]any)
	result["$schema"] = "http://json-schema.org/draft-07/schema#"
	result["title"] = "Stellwerk pipeline document"
	result["definitions"] = definitions
	return result
}

func projectSchema(value any, schemas, definitions map[string]any) any {
	switch node := value.(type) {
	case map[string]any:
		return projectSchemaObject(node, schemas, definitions)
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = projectSchema(child, schemas, definitions)
		}
		return out
	default:
		return value
	}
}

func projectSchemaObject(node, schemas, definitions map[string]any) map[string]any {
	out := map[string]any{}
	for key, child := range node {
		if key == "$ref" {
			out[key] = projectSchemaReference(child.(string), schemas, definitions)
		} else {
			out[key] = projectSchema(child, schemas, definitions)
		}
	}
	return out
}

func projectSchemaReference(reference string, schemas, definitions map[string]any) string {
	name := strings.TrimPrefix(reference, "#/components/schemas/")
	if name == reference {
		panic("non-local pipeline schema reference")
	}
	if _, exists := definitions[name]; !exists {
		definitions[name] = nil
		definitions[name] = projectSchema(schemas[name], schemas, definitions)
	}
	return "#/definitions/" + name
}

func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("generate pipeline document schema: %v", err))
	}
}
