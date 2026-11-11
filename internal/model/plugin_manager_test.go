package model

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPluginManagerActionMustMatchItsFingerprint(t *testing.T) {
	for _, action := range []string{"plugin_package.push", "plugin_installation.upgrade", "plugin_installation.activate"} {
		t.Run(action, func(t *testing.T) {
			_, err := ExecutePluginManagerCommand(t.Context(), nil, PluginManagerCommand{
				Actor: uuid.New(), IdempotencyKey: "request", Action: action,
				Fingerprint: "plugin_installation.enable:sha256:" + strings.Repeat("a", 64), Reason: "Upgrade the plugin",
			}, nil, nil)
			bad, ok := IsErrBadRequest(err)
			require.True(t, ok)
			require.Equal(t, "plugin_manager_request_invalid", bad.Code)
		})
	}
}
