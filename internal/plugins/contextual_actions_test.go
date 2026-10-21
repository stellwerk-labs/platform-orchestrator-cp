package plugins

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContextualActionsRejectURLsAndUndeclaredOperations(t *testing.T) {
	action := ContextualAction{ID: "example.create", ResourceType: "module_version", Label: "Create workflow", RequiredOperationID: "create", Route: ContextualRoute{Path: "new", Query: map[string]string{"module": "$module_id", "version": "$semantic_version"}}, When: ContextualConditions{LifecycleStatus: []string{"proposed"}, SemanticVersionRequired: true}}
	ops := map[string]bool{"create": true}
	require.NoError(t, validateContextualActions([]ContextualAction{action}, ops))
	require.Error(t, validateContextualActions([]ContextualAction{action}, map[string]bool{}))
	require.Error(t, validateContextualActions([]ContextualAction{action, action}, ops))
	for _, path := range []string{"https://example.test/new", "/new", "../new", "new?redirect=evil", "new#fragment", "new//child", "new%2fchild"} {
		t.Run(path, func(t *testing.T) {
			bad := action
			bad.Route.Path = path
			require.Error(t, validateContextualActions([]ContextualAction{bad}, ops))
		})
	}
	bad := action
	bad.Route.Query = map[string]string{"token": "$access_token"}
	require.Error(t, validateContextualActions([]ContextualAction{bad}, ops))
	bad = action
	bad.When.LifecycleStatus = []string{"unknown"}
	require.Error(t, validateContextualActions([]ContextualAction{bad}, ops))
}
