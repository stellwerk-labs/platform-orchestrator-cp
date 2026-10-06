package plugins

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestResourceEventSubscribersComeFromInstalledServiceGrants(t *testing.T) {
	installation := func(id string, orgs []string, actions ...string) Installation {
		item := Installation{ID: id, DisplayName: id, Version: "1.0.0", Backend: "https://" + id + ".example.test",
			Organizations: map[string]AvailabilityState{}, ServiceUserID: userid.NewServiceUserTokenId().String(),
			AdmissionCheckerID: userid.NewServiceUserTokenId().String(),
			Operations:         []Operation{{ID: "list", Method: "GET", Path: "/orgs/{orgId}/items"}}}
		for _, org := range orgs {
			item.Organizations[org] = StateAvailable
		}
		for _, action := range actions {
			item.ServiceGrants = append(item.ServiceGrants, ServiceGrant{Action: action, Permission: "plugin." + id + ".control", Scope: "organization"})
		}
		return item
	}
	encoded, err := json.Marshal([]Installation{
		installation("dev.example.watcher", []string{"acme", "beta"}, ResourceEventsReadAction, "events.publish"),
		installation("dev.example.second", []string{"acme"}, ResourceEventsReadAction),
		installation("dev.example.deployer", []string{"acme", "gamma"}, "external-deployment.admit"),
	})
	require.NoError(t, err)
	registry, err := LoadInstallations(strings.NewReader(string(encoded)), false)
	require.NoError(t, err)

	require.Equal(t, []string{"dev.example.second", "dev.example.watcher"}, registry.ResourceEventSubscribers("acme"))
	require.Equal(t, []string{"dev.example.watcher"}, registry.ResourceEventSubscribers("beta"))
	require.Empty(t, registry.ResourceEventSubscribers("gamma"), "other service grants are not event subscriptions")
	require.Empty(t, registry.ResourceEventSubscribers("unconfigured"))
	require.True(t, registry.HasResourceEventSubscriber("acme"))
	require.False(t, registry.HasResourceEventSubscriber("gamma"))

	var absent *InstallationRegistry
	require.Empty(t, absent.ResourceEventSubscribers("acme"))
	require.False(t, absent.HasResourceEventSubscriber("acme"), "a Core-only server has no subscribers")
}
