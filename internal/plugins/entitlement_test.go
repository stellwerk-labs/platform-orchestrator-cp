package plugins

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	registry, err := NewInstallationRegistry([]Installation{item}, false)
	require.NoError(t, err)
	served := func() Installation {
		served, _, found := registry.Installation(context.Background(), "acme", item.ID)
		require.True(t, found)
		return served
	}
	snapshot := served()
	snapshot.Entitlement.Features[0] = "another-feature"
	snapshot.Organizations["acme"] = StateNotEnabled
	snapshot.Operations[0].RequiredFeature = "another-feature"
	require.Equal(t, "workflow", served().Entitlement.Features[0])
	require.Equal(t, StateAvailable, served().Organizations["acme"])
	require.Equal(t, "workflow", served().Operations[0].RequiredFeature)
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
	registry, err := NewInstallationRegistry([]Installation{item}, false)
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

// fixedEntitlements is the evaluated input Core's adapter would return; the
// adapter itself is covered against PostgreSQL by integration tests.
type fixedEntitlements EntitlementStatus

func (f fixedEntitlements) ResolveEntitlement(context.Context, string, Installation) (EntitlementStatus, error) {
	return EntitlementStatus(f), nil
}

func TestGraceKeepsForwardWorkAndWarns(t *testing.T) {
	item := licensedInstallation()
	item.Entitlement.Features = []string{"audit", "workflow", "zeta"}
	early, late := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	status := EntitlementStatus{Features: map[string]FeatureAccess{
		"zeta":     {State: "grace", Forward: true, Read: true, Safety: true, GraceUntil: &late},
		"workflow": {State: "grace", Forward: true, Read: true, Safety: true, GraceUntil: &early},
		"audit":    {State: "valid", Forward: true, Read: true, Safety: true},
	}}
	d := projectEntitlement(item, Descriptor{State: StateAvailable, Mode: ModeFull}, status)
	require.Equal(t, StateAvailable, d.State)
	require.Equal(t, ModeFull, d.Mode)
	require.True(t, d.Allows("workflow", true, false), "grace must not block forward work")
	require.Equal(t, &EntitlementWarning{
		Code: EntitlementGraceWarning, GraceUntil: early, Features: []string{"workflow", "zeta"},
		Message: "The signed plugin licence is in its grace period until 2026-10-08T12:00:00Z. Forward work continues until then; ask an administrator to renew the licence.",
	}, d.Entitlement.Warning)
	require.Equal(t, d.Entitlement.Warning.Message, d.AdminAction)
	require.Nil(t, d.Entitlement.GraceWarning("audit"), "a valid feature carries no warning")
	require.Equal(t, late, d.Entitlement.GraceWarning("zeta").GraceUntil)

	valid := projectEntitlement(item, Descriptor{State: StateAvailable, Mode: ModeFull}, EntitlementStatus{Features: map[string]FeatureAccess{"workflow": {State: "valid", Forward: true, Read: true, Safety: true}}})
	require.Nil(t, valid.Entitlement.Warning)
	require.Empty(t, valid.AdminAction)
	var none *EntitlementStatus
	require.Nil(t, none.GraceWarning("workflow"))
}

func TestGatewayResultCarriesGraceWarning(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer backend.Close()
	item := licensedInstallation()
	item.Backend = backend.URL
	registry, err := NewInstallationRegistry([]Installation{item}, true)
	require.NoError(t, err)
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	registry.Entitlements = fixedEntitlements{Features: map[string]FeatureAccess{"workflow": {State: "grace", Forward: true, Read: true, Safety: true, GraceUntil: &until}}}
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: "cp"}
	result, err := gateway.Invoke(t.Context(), "acme", item.ID, testActor, Invocation{OperationID: "read"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, result.Status)
	require.NotNil(t, result.EntitlementWarning)
	require.Equal(t, EntitlementGraceWarning, result.EntitlementWarning.Code)
	require.Equal(t, until, result.EntitlementWarning.GraceUntil)

	registry.Entitlements = fixedEntitlements{Features: map[string]FeatureAccess{"workflow": {State: "valid", Forward: true, Read: true, Safety: true}}}
	result, err = gateway.Invoke(t.Context(), "acme", item.ID, testActor, Invocation{OperationID: "read"})
	require.NoError(t, err)
	require.Nil(t, result.EntitlementWarning)
}
