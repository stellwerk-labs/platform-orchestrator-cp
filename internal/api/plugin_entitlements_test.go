package api

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stretchr/testify/require"
)

func TestPluginEffectRejectsUnsafeTransactionOptionsBeforeOpeningDatabase(t *testing.T) {
	for _, options := range []sql.TxOptions{
		{ReadOnly: true},
		{Isolation: sql.LevelReadCommitted, ReadOnly: true},
		{Isolation: sql.LevelReadUncommitted},
		{Isolation: sql.LevelWriteCommitted},
		{Isolation: sql.LevelRepeatableRead},
		{Isolation: sql.LevelSnapshot},
		{Isolation: sql.LevelSerializable},
		{Isolation: sql.LevelLinearizable},
		{Isolation: sql.IsolationLevel(100)},
	} {
		t.Run(options.Isolation.String(), func(t *testing.T) {
			server := &Server{}
			tx, err := server.pluginEffectDatabase("acme", plugins.Installation{}, "effect.commit", false).BeginTx(t.Context(), &options)
			require.Nil(t, tx)
			var denied *herrors.PlatformOrchestratorError
			require.ErrorAs(t, err, &denied)
			require.Equal(t, "plugin_effect_transaction_invalid", denied.Code)
		})
	}
}

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

func TestPluginEntitlementGraceWarningReachesTheWire(t *testing.T) {
	until := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	status := &plugins.EntitlementStatus{Features: map[string]plugins.FeatureAccess{"workflow": {State: "grace", Forward: true, Read: true, Safety: true, GraceUntil: &until}}}
	status.Warning = status.GraceWarning("workflow")
	projection := pluginEntitlementToAPI(status)
	require.NotNil(t, projection.Warning)
	require.Equal(t, PluginEntitlementWarningCodeGrace, projection.Warning.Code)
	require.Equal(t, until, projection.Warning.GraceUntil)
	require.Equal(t, []string{"workflow"}, projection.Warning.Features)
	encoded, err := json.Marshal(projection)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"warning":{"code":"plugin_entitlement_grace"`)

	item := plugins.Installation{ServiceGrants: []plugins.ServiceGrant{{Action: "effect.commit", RequiredFeature: "workflow"}}}
	descriptor := plugins.Descriptor{Entitlement: status}
	require.Equal(t, projection.Warning, pluginServiceWarning(item, descriptor, "effect.commit"))
	require.Nil(t, pluginServiceWarning(item, descriptor, "undeclared"))
	require.Nil(t, pluginServiceWarning(item, plugins.Descriptor{}, "effect.commit"))
}
