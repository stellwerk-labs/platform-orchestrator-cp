package plugins

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaticRegistryReturnsDefensiveCapabilityCopies(t *testing.T) {
	descriptor := Descriptor{ID: "dev.stellwerk.example", Capabilities: []string{"example.read"}, State: StateAvailable}
	registry := NewStaticRegistry(descriptor)

	first, found := registry.Get(t.Context(), "acme", descriptor.ID)
	require.True(t, found)
	first.Capabilities[0] = "corrupted"

	second, found := registry.Get(t.Context(), "acme", descriptor.ID)
	require.True(t, found)
	assert.Equal(t, []string{"example.read"}, second.Capabilities)
}
