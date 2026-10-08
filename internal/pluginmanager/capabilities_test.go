package pluginmanager

import (
	"strings"
	"testing"
	"unicode/utf8"

	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/require"
)

func TestReconciliationFailuresStayBoundedAndReadable(t *testing.T) {
	require.Equal(t, "IAM answered 503", truncate("IAM answered 503", maxFailure))
	long := strings.Repeat("é", maxFailure)
	cut := truncate(long, maxFailure)
	require.LessOrEqual(t, len(cut), maxFailure)
	require.Equal(t, maxFailure/2, utf8.RuneCountInString(cut))
	require.True(t, utf8.ValidString(cut), "a cut never splits a character")
	require.Equal(t, "ab", truncate("abé", 3))
}

func TestRegistrationsCompareScopesAsASet(t *testing.T) {
	read := registration("read", "Read", "View.", "Workflow", "read", []orchestratoriam.PluginCapabilityRegistrationScopes{"organization", "environment"})
	reordered := read
	reordered.Scopes = []orchestratoriam.PluginCapabilityRegistrationScopes{"environment", "organization"}
	require.True(t, sameRegistration(read, reordered))
	require.Equal(t, []orchestratoriam.PluginCapabilityRegistrationScopes{"organization", "environment"}, read.Scopes, "comparing never reorders the registration")
	for name, change := range map[string]func(*orchestratoriam.PluginCapabilityRegistration){
		"name":         func(r *orchestratoriam.PluginCapabilityRegistration) { r.Name = "list" },
		"display name": func(r *orchestratoriam.PluginCapabilityRegistration) { r.DisplayName = "List" },
		"description":  func(r *orchestratoriam.PluginCapabilityRegistration) { r.Description = "List." },
		"category":     func(r *orchestratoriam.PluginCapabilityRegistration) { r.Category = "Other" },
		"level":        func(r *orchestratoriam.PluginCapabilityRegistration) { r.Level = "write" },
		"scopes": func(r *orchestratoriam.PluginCapabilityRegistration) {
			r.Scopes = []orchestratoriam.PluginCapabilityRegistrationScopes{"organization"}
		},
	} {
		changed := read
		changed.Scopes = append([]orchestratoriam.PluginCapabilityRegistrationScopes{}, read.Scopes...)
		change(&changed)
		require.False(t, sameRegistration(read, changed), name)
	}
}
