package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

func decodePipelineDocument(raw []byte, kind string) (any, error) {
	switch kind {
	case "application/json":
		return decodePipelineJSON(raw)
	case "application/yaml":
		return decodePipelineYAML(raw)
	default:
		return nil, errors.New("unsupported format")
	}
}

func decodePipelineJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	value, err := pipelineJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, last := decoder.Token(); !errors.Is(last, io.EOF) {
		return nil, errors.New("multiple documents")
	}
	return value, nil
}

func decodePipelineYAML(raw []byte) (any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	value, err := pipelineYAMLValue(&node, 0)
	if err != nil {
		return nil, err
	}
	var extra yaml.Node
	if last := decoder.Decode(&extra); !errors.Is(last, io.EOF) {
		return nil, errors.New("multiple documents")
	}
	return value, nil
}

func pipelineJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxPipelineDocumentDepth {
		return nil, errors.New("nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		return pipelineJSONObject(decoder, depth)
	case '[':
		return pipelineJSONArray(decoder, depth)
	default:
		return nil, errors.New("unexpected delimiter")
	}
}

func pipelineJSONObject(decoder *json.Decoder, depth int) (any, error) {
	value := map[string]any{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("non-string key")
		}
		if _, exists := value[name]; exists {
			return nil, errors.New("duplicate key")
		}
		child, err := pipelineJSONValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		value[name] = child
	}
	_, err := decoder.Token()
	return value, err
}

func pipelineJSONArray(decoder *json.Decoder, depth int) (any, error) {
	value := []any{}
	for decoder.More() {
		child, err := pipelineJSONValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		value = append(value, child)
	}
	_, err := decoder.Token()
	return value, err
}

func pipelineYAMLValue(node *yaml.Node, depth int) (any, error) {
	if depth > maxPipelineDocumentDepth || node.Anchor != "" || node.Kind == yaml.AliasNode || node.Style&yaml.TaggedStyle != 0 {
		return nil, errors.New("unsupported YAML node")
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) != 1 {
			return nil, errors.New("one document required")
		}
		return pipelineYAMLValue(node.Content[0], depth+1)
	case yaml.MappingNode:
		return pipelineYAMLMapping(node, depth)
	case yaml.SequenceNode:
		return pipelineYAMLSequence(node, depth)
	case yaml.ScalarNode:
		return pipelineYAMLScalar(node)
	default:
		return nil, errors.New("unsupported YAML value")
	}
}

func pipelineYAMLMapping(node *yaml.Node, depth int) (any, error) {
	value := map[string]any{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || key.Anchor != "" || key.Style&yaml.TaggedStyle != 0 {
			return nil, errors.New("string keys required")
		}
		if _, exists := value[key.Value]; exists {
			return nil, errors.New("duplicate key")
		}
		child, err := pipelineYAMLValue(node.Content[i+1], depth+1)
		if err != nil {
			return nil, err
		}
		value[key.Value] = child
	}
	return value, nil
}

func pipelineYAMLSequence(node *yaml.Node, depth int) (any, error) {
	value := []any{}
	for _, child := range node.Content {
		item, err := pipelineYAMLValue(child, depth+1)
		if err != nil {
			return nil, err
		}
		value = append(value, item)
	}
	return value, nil
}

func pipelineYAMLScalar(node *yaml.Node) (any, error) {
	switch node.Tag {
	case "!!str", "!!bool", "!!int", "!!float", "!!null":
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(raw, &value)
		return value, err
	default:
		return nil, errors.New("unsupported YAML value")
	}
}
