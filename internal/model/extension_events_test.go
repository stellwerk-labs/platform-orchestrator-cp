package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtensionEventTypeHasUnambiguousPluginOwnership(t *testing.T) {
	require.True(t, validExtensionEventType("dev.example", "plugin.dev.example.changed"))
	require.True(t, validExtensionEventType("dev.example.child", "plugin.dev.example.child.changed"))
	require.False(t, validExtensionEventType("dev.example", "plugin.dev.example.child.changed"), "a parent plugin cannot publish as a nested plugin")
	require.False(t, validExtensionEventType("dev.example.child", "plugin.dev.example.changed"))
	require.True(t, validExtensionEventType("dev.stellwerk.progressive-rollouts", "plugin.dev.stellwerk.progressive-rollouts.rollout-event"))
	for _, suffix := range []string{"", "nested.changed", ".changed", "changed.", "changed..again", "*", ">", "bad/name", "bad\nname", "1changed", strings.Repeat("a", 201)} {
		require.False(t, validExtensionEventType("dev.example", "plugin.dev.example."+suffix), suffix)
	}
}
