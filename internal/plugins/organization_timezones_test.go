package plugins

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallationOrganizationTimezones(t *testing.T) {
	for _, test := range []struct {
		name  string
		zones map[string]string
		valid bool
		want  string
	}{
		{"default is explicit UTC", nil, true, "UTC"},
		{"IANA daylight savings", map[string]string{"acme": "Europe/Berlin"}, true, "Europe/Berlin"},
		{"invalid", map[string]string{"acme": "Mars/Olympus"}, false, ""},
		{"local depends on host", map[string]string{"acme": "Local"}, false, ""},
		{"unknown org", map[string]string{"other": "UTC"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := Installation{ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.test", Organizations: map[string]AvailabilityState{"acme": StateAvailable}, OrganizationTimezones: test.zones}
			registry, err := NewInstallationRegistry([]Installation{item}, false)
			if !test.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			loaded, _, found := registry.Installation(t.Context(), "acme", item.ID)
			require.True(t, found)
			require.Equal(t, test.want, loaded.OrganizationTimezones["acme"])
		})
	}
}
