package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func licensedInstallation() Installation {
	return Installation{
		ID: "dev.example.workflow", DisplayName: "Workflow", Version: "1.0.0", Backend: "https://workflow.example.test",
		InstallationID: uuid.NewString(), ManifestSHA256: strings.Repeat("a", 64),
		Organizations: map[string]AvailabilityState{"acme": StateAvailable},
		Entitlement:   &EntitlementDeclaration{Mode: "required", Features: []string{"workflow"}, Limits: []string{}},
		Operations:    []Operation{{ID: "read", Method: "GET", Path: "/orgs/{orgId}/state", RequiredFeature: "workflow"}},
	}
}

func TestInstallationSnapshotCannotMutateEntitlementBoundary(t *testing.T) {
	item := licensedInstallation()
	registry := &InstallationRegistry{installations: map[string]Installation{item.ID: item}}
	snapshot := registry.Installations()
	require.Len(t, snapshot, 1)
	snapshot[0].Entitlement.Features[0] = "another-feature"
	snapshot[0].Organizations["acme"] = StateNotEnabled
	snapshot[0].Operations[0].RequiredFeature = "another-feature"
	require.Equal(t, "workflow", registry.Installations()[0].Entitlement.Features[0])
	require.Equal(t, StateAvailable, registry.Installations()[0].Organizations["acme"])
	require.Equal(t, "workflow", registry.Installations()[0].Operations[0].RequiredFeature)
}

func TestEntitlementDeclarationClosedRequirements(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Installation)
	}{
		{"mode", func(i *Installation) { i.Entitlement.Mode = "available" }},
		{"identity missing", func(i *Installation) { i.InstallationID = "" }},
		{"identity zero", func(i *Installation) { i.InstallationID = uuid.Nil.String() }},
		{"manifest missing", func(i *Installation) { i.ManifestSHA256 = "" }},
		{"manifest noncanonical", func(i *Installation) { i.ManifestSHA256 = strings.Repeat("A", 64) }},
		{"features empty", func(i *Installation) { i.Entitlement.Features = nil }},
		{"features unsorted", func(i *Installation) { i.Entitlement.Features = []string{"workflow", "other"} }},
		{"features duplicate", func(i *Installation) { i.Entitlement.Features = []string{"workflow", "workflow"} }},
		{"features unknown format", func(i *Installation) { i.Entitlement.Features = []string{"plugin.other.feature"} }},
		{"unsupported limits", func(i *Installation) { i.Entitlement.Limits = []string{"environments"} }},
		{"operation missing feature", func(i *Installation) { i.Operations[0].RequiredFeature = "" }},
		{"operation foreign feature", func(i *Installation) { i.Operations[0].RequiredFeature = "other" }},
		{"service missing feature", func(i *Installation) { i.ServiceGrants = []ServiceGrant{{Action: "read"}} }},
		{"unlicensed declaration", func(i *Installation) { i.Entitlement.Mode = "none" }},
		{"unlicensed operation", func(i *Installation) { i.Entitlement = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := licensedInstallation()
			test.edit(&item)
			require.Error(t, validateEntitlement(item))
		})
	}
	require.NoError(t, validateEntitlement(licensedInstallation()))
}

func TestRequiredEntitlementFailsClosedWithoutVerifier(t *testing.T) {
	item := licensedInstallation()
	encoded, err := json.Marshal([]Installation{item})
	require.NoError(t, err)
	registry, err := LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)
	descriptor, found := registry.Get(context.Background(), "acme", item.ID)
	require.True(t, found)
	require.Equal(t, StateNotEntitled, descriptor.State)
	require.Equal(t, ModeHidden, descriptor.Mode)
	require.False(t, descriptor.Allows("workflow", false, false))
	require.False(t, descriptor.Allows("workflow", true, true))
	require.False(t, descriptor.Allows("workflow", true, false))
}

func TestEntitlementAccessSeparatesForwardHistoryAndSafety(t *testing.T) {
	item := licensedInstallation()
	for _, test := range []struct {
		name                  string
		base                  AvailabilityState
		access                FeatureAccess
		state                 AvailabilityState
		mode                  AccessMode
		forward, read, safety bool
	}{
		{"valid", StateAvailable, FeatureAccess{Forward: true, Read: true, Safety: true}, StateAvailable, ModeFull, true, true, true},
		{"missing", StateAvailable, FeatureAccess{}, StateNotEntitled, ModeHidden, false, false, false},
		{"expired activated", StateAvailable, FeatureAccess{State: "expired", Read: true, Safety: true}, StateNotEntitled, ModeSafetyOnly, false, true, true},
		{"expired never activated", StateAvailable, FeatureAccess{State: "expired"}, StateNotEntitled, ModeHidden, false, false, false},
		{"disabled", StateNotEnabled, FeatureAccess{Forward: true, Read: true, Safety: true}, StateNotEnabled, ModeSafetyOnly, false, true, true},
		{"commercial deny", StateNotEntitled, FeatureAccess{Forward: true, Read: true, Safety: true}, StateNotEntitled, ModeSafetyOnly, false, true, true},
		{"incompatible", StateIncompatible, FeatureAccess{Forward: true, Read: true, Safety: true}, StateIncompatible, ModeHidden, false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := projectEntitlement(item, Descriptor{State: test.base, Mode: accessMode(test.base)}, EntitlementStatus{Features: map[string]FeatureAccess{
				"workflow": test.access, "undeclared": {Forward: true, Read: true, Safety: true},
			}})
			require.Equal(t, test.state, d.State)
			require.Equal(t, test.mode, d.Mode)
			require.Equal(t, test.forward, d.Allows("workflow", true, false))
			require.Equal(t, test.read, d.Allows("workflow", false, false))
			require.Equal(t, test.safety, d.Allows("workflow", true, true))
			require.NotContains(t, d.Entitlement.Features, "undeclared")
			require.False(t, d.Allows("undeclared", true, false))
		})
	}
}
