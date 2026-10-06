package api

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

func TestPluginResourceEventErrorSeparatesExpiredCursors(t *testing.T) {
	var failure *herrors.PlatformOrchestratorError
	require.ErrorAs(t, pluginResourceEventError(fmt.Errorf("read: %w", model.ErrExtensionResourceCursorExpired)), &failure)
	require.Equal(t, 409, failure.StatusCode)
	require.Equal(t, "plugin_resource_cursor_expired", failure.Code)

	require.ErrorAs(t, pluginResourceEventError(model.NewErrConflict("changed")), &failure)
	require.Equal(t, "plugin_operation_conflict", failure.Code, "ordinary conflicts keep their existing code")
	require.ErrorAs(t, pluginResourceEventError(sql.ErrNoRows), &failure)
	require.Equal(t, 404, failure.StatusCode)
}
