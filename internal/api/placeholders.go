package api

import (
	"slices"

	"github.com/pkg/errors"
	po_graph "github.com/stellwerk-labs/platform-orchestrator-graph"
)

var PlaceholdersSupportedInModule = []po_graph.PlaceholderType{
	po_graph.PlaceholderTypeContext,
	po_graph.PlaceholderTypeResource,
	po_graph.PlaceholderTypeSelector,
	po_graph.PlaceholderTypeSelf,
}

var PlaceholdersSupportedInProvider = []po_graph.PlaceholderType{
	po_graph.PlaceholderTypeContext,
	po_graph.PlaceholderTypeTfVar,
	po_graph.PlaceholderTypeResource,
}

const projectIDContextKey = "project_id"

var supportedContextKeys = map[string]bool{
	"org_id":            true,
	projectIDContextKey: true,
	"env_id":            true,
	"env_type_id":       true,
	"res_type":          true,
	"res_class":         true,
	"res_id":            true,
}

// ValidatePlaceholderSyntax performs some very basic validation of syntax and supported types.
func ValidatePlaceholderSyntax(raw map[string]interface{}, allowedTypes []po_graph.PlaceholderType) error {
	var err error
	for _, sub := range po_graph.IterPlaceholders(raw, &err) {
		if !slices.Contains(allowedTypes, sub.Type()) {
			return errors.Errorf("'%s' placeholders are not supported here", sub.Type())
		}
		if pc, ok := sub.(*po_graph.ContextPlaceholder); ok && !supportedContextKeys[pc.Key] {
			return errors.Errorf("'%s' is not a supported context placeholder key", pc.Key)
		}
	}
	return err
}
