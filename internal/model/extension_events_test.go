package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
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

func TestExtensionJournalEntriesOnlyForSubscribedOrganizations(t *testing.T) {
	message := func(subject genevents.EventType, org string) *hstandardoutbox.PendingEventMessage {
		envID := uuid.New()
		payload, err := json.Marshal(map[string]any{"time": time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
			"data": map[string]any{"org_id": org, "env_uuid": envID}})
		require.NoError(t, err)
		return &hstandardoutbox.PendingEventMessage{Subject: string(subject), Payload: payload}
	}
	messages := []*hstandardoutbox.PendingEventMessage{
		message(genevents.IoPlatformOrchestratorEnvironmentDeleted, "subscribed"),
		message(genevents.IoPlatformOrchestratorEnvironmentDeleted, "unsubscribed"),
		message(genevents.IoPlatformOrchestratorModuleCatalogueChanged, "subscribed"),
		{Subject: "io.platform-orchestrator.deployment.started", Payload: []byte(`{"data":{"org_id":"subscribed"}}`)},
	}
	entries, err := extensionJournalEntries(messages, func(org string) bool { return org == "subscribed" })
	require.NoError(t, err)
	require.Len(t, entries, 2, "only journal event types of subscribed organizations are projected")
	for _, entry := range entries {
		require.Equal(t, "subscribed", entry.orgID)
	}
	require.Equal(t, string(genevents.IoPlatformOrchestratorEnvironmentDeleted), entries[0].event.Type)
	require.Equal(t, string(genevents.IoPlatformOrchestratorModuleCatalogueChanged), entries[1].event.Type)

	none, err := extensionJournalEntries(messages, func(string) bool { return false })
	require.NoError(t, err)
	require.Empty(t, none)

	_, err = extensionJournalEntries([]*hstandardoutbox.PendingEventMessage{{Subject: string(genevents.IoPlatformOrchestratorEnvironmentDeleted), Payload: []byte(`{"data":{}}`)}}, func(string) bool { return true })
	require.Error(t, err, "a journal event without an organization is rejected")
}
