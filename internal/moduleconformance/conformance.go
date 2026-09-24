// Package moduleconformance validates author-declared Orchestrator interfaces.
// It never resolves artifacts, evaluates placeholders or verifies runtime outputs.
package moduleconformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

const (
	CodeInvalidContract = "resource_type_contract_invalid"
	CodeNonconformant   = "module_resource_type_nonconformant"
	CodeOutputRequired  = "module_output_declaration_required"
	CodeOutputMismatch  = "module_output_contract_mismatch"
	MaxContractBytes    = 64 * 1024
	MaxContractDepth    = 16
	MaxContractNodes    = 64
	contractPath        = "/module_contract"
	outputPath          = "/output_schema"
	ruleJSON            = "json"
)

// Error deliberately retains no failing values, schema text or wrapped errors.
// Path is a JSON pointer; Rule is a stable schema keyword or validation rule.
type Error struct {
	Code string
	Path string
	Rule string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s at %s (%s)", e.Code, e.Path, e.Rule)
}

// ValidateContract accepts nil for a retained Resource Type without a contract.
// A present contract must be an explicit object schema, not an empty map.
func ValidateContract(ctx context.Context, contract map[string]any) error {
	_, err := compileContract(ctx, contract)
	return err
}

// ValidateDefinition checks the complete declaration projection without mutation.
// Callers supply effective optional booleans and explicit empty maps/arrays.
// Only an absent output declaration is omitted from that projection.
func ValidateDefinition(ctx context.Context, contract map[string]any, definition map[string]any) error {
	schema, err := compileContract(ctx, contract)
	if err != nil || schema == nil {
		return err
	}
	// Validate a JSON-normalised copy, so integer representations behave like API
	// input and neither validation nor future library defaults can mutate callers.
	raw, err := json.Marshal(definition)
	if err != nil {
		return &Error{Code: CodeNonconformant, Rule: ruleJSON}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return &Error{Code: CodeNonconformant, Rule: ruleJSON}
	}
	if err := schema.VisitJSON(value, openapi3.MultiErrors(),
		openapi3.SetSchemaErrorMessageCustomizer(func(*openapi3.SchemaError) string {
			return "module declaration does not conform"
		})); err != nil {
		return definitionError(err)
	}
	return nil
}

// ValidateOutputs compares declarations as JSON values, not as compatible
// schemas. A nil declaration is historical/omitted, distinct from explicit {}.
func ValidateOutputs(required, declared map[string]any) error {
	if len(required) == 0 {
		return nil
	}
	if declared == nil {
		return &Error{Code: CodeOutputRequired, Path: outputPath, Rule: "required"}
	}
	expected, err := decodeJSON(required)
	if err != nil {
		return &Error{Code: CodeOutputMismatch, Path: outputPath, Rule: ruleJSON}
	}
	actual, err := decodeJSON(declared)
	if err != nil {
		return &Error{Code: CodeOutputMismatch, Path: outputPath, Rule: ruleJSON}
	}
	if !equalJSON(expected, actual) {
		return &Error{Code: CodeOutputMismatch, Path: outputPath, Rule: "equality"}
	}
	return nil
}

func decodeJSON(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalised any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&normalised); err != nil {
		return nil, err
	}
	return normalised, nil
}

func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		left, validLeft := new(big.Rat).SetString(string(a))
		right, validRight := new(big.Rat).SetString(string(b))
		return validLeft && validRight && left.Cmp(right) == 0
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, exists := b[key]
			if !exists || !equalJSON(value, other) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, value := range a {
			if !equalJSON(value, b[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func compileContract(ctx context.Context, contract map[string]any) (*openapi3.Schema, error) {
	if contract == nil {
		return nil, nil
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		return nil, invalid(contractPath, ruleJSON)
	}
	if len(raw) > MaxContractBytes {
		return nil, invalid(contractPath, "maxBytes")
	}
	var normalised map[string]any
	if err := json.Unmarshal(raw, &normalised); err != nil {
		return nil, invalid(contractPath, ruleJSON)
	}
	if normalised["type"] != "object" {
		return nil, invalid(contractPath+"/type", "type")
	}
	walker := contractWalker{}
	if err := walker.visit(normalised, contractPath, 1, true); err != nil {
		return nil, err
	}
	var schema openapi3.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, invalid(contractPath, "schema")
	}
	// Ref resolution is impossible: the walker rejected refs before decoding.
	if err := schema.Validate(ctx); err != nil {
		return nil, invalid(contractPath, "schema")
	}
	return &schema, nil
}

func invalid(path, rule string) *Error {
	return &Error{Code: CodeInvalidContract, Path: path, Rule: rule}
}

type contractWalker struct{ nodes int }

func (w *contractWalker) visit(schema map[string]any, path string, depth int, root bool) error {
	if depth > MaxContractDepth {
		return invalid(path, "maxDepth")
	}
	w.nodes++
	if w.nodes > MaxContractNodes {
		return invalid(path, "maxNodes")
	}
	for _, key := range slices.Sorted(maps.Keys(schema)) {
		value := schema[key]
		field := pointer(path, key)
		switch key {
		case "type":
			name, ok := value.(string)
			if !ok || !slices.Contains([]string{"object", "array", "string", "number", "integer", "boolean"}, name) {
				return invalid(field, "type")
			}
			if name == "array" && schema["items"] == nil {
				return invalid(pointer(path, "items"), "required")
			}
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok {
				return invalid(field, "type")
			}
			for _, name := range slices.Sorted(maps.Keys(properties)) {
				if root && !definitionField(name) {
					return invalid(pointer(field, name), "definition_field")
				}
				if err := w.child(properties[name], pointer(field, name), depth+1, false); err != nil {
					return err
				}
			}
		case "required":
			values, ok := value.([]any)
			if !ok {
				return invalid(field, "type")
			}
			seen := make(map[string]bool)
			for i, item := range values {
				name, ok := item.(string)
				if !ok {
					return invalid(pointer(field, strconv.Itoa(i)), "type")
				}
				if seen[name] {
					return invalid(field, "uniqueItems")
				}
				seen[name] = true
				if root && !definitionField(name) {
					return invalid(pointer(field, strconv.Itoa(i)), "definition_field")
				}
			}
		case "additionalProperties":
			if _, boolean := value.(bool); !boolean {
				if err := w.child(value, field, depth+1, false); err != nil {
					return err
				}
			}
		case "items":
			if err := w.child(value, field, depth+1, false); err != nil {
				return err
			}
		case "not":
			if err := w.child(value, field, depth+1, root); err != nil {
				return err
			}
		case "allOf":
			items, ok := value.([]any)
			if !ok || len(items) == 0 {
				return invalid(field, "schema_array")
			}
			for i, item := range items {
				if err := w.child(item, pointer(field, strconv.Itoa(i)), depth+1, root); err != nil {
					return err
				}
			}
		case "enum":
			items, ok := value.([]any)
			if !ok || len(items) == 0 {
				return invalid(field, "nonempty_array")
			}
		case "nullable", "exclusiveMinimum", "exclusiveMaximum", "uniqueItems":
			if _, ok := value.(bool); !ok {
				return invalid(field, "type")
			}
		case "minimum", "maximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
			number, ok := value.(float64)
			if !ok {
				return invalid(field, "type")
			}
			if key == "multipleOf" && number <= 0 {
				return invalid(field, "positive")
			}
			if strings.HasPrefix(key, "min") && key != "minimum" || strings.HasPrefix(key, "max") && key != "maximum" {
				if number < 0 || number != math.Trunc(number) {
					return invalid(field, "nonnegative_integer")
				}
			}
		case "pattern", "title", "description":
			text, ok := value.(string)
			if !ok {
				return invalid(field, "type")
			}
			if key == "pattern" {
				if _, err := regexp.Compile(text); err != nil {
					return invalid(field, "pattern")
				}
			}
		default:
			return invalid(field, "unsupported_keyword")
		}
	}
	return nil
}

func (w *contractWalker) child(value any, path string, depth int, root bool) error {
	schema, ok := value.(map[string]any)
	if !ok {
		return invalid(path, "schema_object")
	}
	return w.visit(schema, path, depth, root)
}

func definitionField(name string) bool {
	return slices.Contains([]string{"module_inputs", "module_params", "provider_mapping", "dependencies", "coprovisioned", "output_schema"}, name)
}

func pointer(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func definitionError(err error) *Error {
	var violations []*Error
	var collect func(error)
	collect = func(err error) {
		switch typed := err.(type) {
		case openapi3.MultiError:
			for _, child := range typed {
				collect(child)
			}
		case *openapi3.SchemaError:
			path := ""
			for _, part := range typed.JSONPointer() {
				path = pointer(path, part)
			}
			violations = append(violations, &Error{Code: CodeNonconformant, Path: path, Rule: typed.SchemaField})
		}
	}
	collect(err)
	if len(violations) == 0 {
		return &Error{Code: CodeNonconformant, Rule: "schema"}
	}
	slices.SortFunc(violations, func(a, b *Error) int {
		if compared := strings.Compare(a.Path, b.Path); compared != 0 {
			return compared
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	return violations[0]
}
