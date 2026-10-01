package api

import (
	"testing"

	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stretchr/testify/require"
)

func TestPluginFeatureAccessNeverUsesAnotherFeaturesLicence(t *testing.T) {
	descriptor := plugins.Descriptor{State: plugins.StateAvailable, Mode: plugins.ModeFull, Entitlement: &plugins.EntitlementStatus{Features: map[string]plugins.FeatureAccess{
		"active":  {Forward: true, Read: true, Safety: true},
		"expired": {Read: true, Safety: true},
		"unused":  {},
	}}}
	for _, test := range []struct {
		name, feature    string
		mutating, safety bool
		allowed          bool
	}{
		{"active forward", "active", true, false, true},
		{"active read", "active", false, false, true},
		{"expired forward", "expired", true, false, false},
		{"expired safety", "expired", true, true, true},
		{"expired history", "expired", false, false, true},
		{"never activated history", "unused", false, false, false},
		{"never activated safety", "unused", true, true, false},
		{"unknown feature", "unknown", true, true, false},
		{"missing feature", "", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := pluginFeatureAllowed(descriptor, test.feature, test.mutating, test.safety)
			if test.allowed {
				require.NoError(t, err)
				return
			}
			var denied *herrors.PlatformOrchestratorError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "plugin_not_entitled", denied.Code)
			require.Equal(t, 409, denied.StatusCode)
		})
	}
}

func TestPluginEffectRequiresConfiguredVerifier(t *testing.T) {
	item := plugins.Installation{Entitlement: &plugins.EntitlementDeclaration{Mode: "required", Features: []string{"workflow"}}, ServiceGrants: []plugins.ServiceGrant{{Action: "module.promote", RequiredFeature: "workflow"}}}
	server := &Server{}
	tx, err := server.pluginEffectDatabase("acme", item, "module.promote", false).BeginTx(t.Context(), nil)
	require.Nil(t, tx)
	var denied *herrors.PlatformOrchestratorError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "plugin_not_entitled", denied.Code)
}

func TestPluginEntitlementProjectionHasNoRawEvidence(t *testing.T) {
	projection := pluginEntitlementToAPI(&plugins.EntitlementStatus{Features: map[string]plugins.FeatureAccess{"workflow": {State: "grace", TrustClass: "development", Read: true, Safety: true}}})
	require.Equal(t, "development", projection.Features["workflow"].TrustClass)
	require.False(t, projection.Features["workflow"].Forward)
	require.True(t, projection.Features["workflow"].Safety)
	require.Nil(t, pluginEntitlementToAPI(nil))
}
