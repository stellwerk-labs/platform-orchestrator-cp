package plugins

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallationRequiresCanonicalVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		valid   bool
	}{
		{"0.0.0", true},
		{"1.2.3", true},
		{"0.2.0-native.20260928", true},
		{"1.2.3-alpha.0.1a+build.001", true},
		{"", false},
		{"latest", false},
		{"v1.2.3", false},
		{"1.2", false},
		{"01.2.3", false},
		{"1.02.3", false},
		{"1.2.03", false},
		{"1.2.3-01", false},
		{"1.2.3-alpha.01", false},
		{"1.2.3-alpha..1", false},
		{"1.2.3+build..1", false},
		{"1.2.3-", false},
		{"1.2.3+", false},
		{"1.2.3\n", false},
	} {
		t.Run(test.version, func(t *testing.T) {
			item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: test.version, Backend: "https://workflow.test", Organizations: map[string]AvailabilityState{"acme": StateAvailable}}
			encoded, err := json.Marshal([]Installation{item})
			require.NoError(t, err)
			registry, err := LoadInstallations(bytes.NewReader(encoded), false)
			if !test.valid {
				require.ErrorContains(t, err, "invalid plugin identity")
				return
			}
			require.NoError(t, err)
			loaded, _, found := registry.Installation(t.Context(), "acme", item.ID)
			require.True(t, found)
			require.Equal(t, test.version, loaded.Version)
		})
	}
}
