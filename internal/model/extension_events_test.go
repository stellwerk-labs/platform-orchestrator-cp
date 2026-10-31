package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genevents"
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

// Whether an organization journals its entries is its journal state, read
// when the transaction commits; projection keeps every organization.
func TestExtensionJournalEntriesProjectEveryOrganization(t *testing.T) {
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
	entries, err := extensionJournalEntries(messages)
	require.NoError(t, err)
	projected := []string{}
	for _, entry := range entries {
		projected = append(projected, entry.orgID+" "+entry.event.Type)
	}
	require.Equal(t, []string{
		"subscribed " + string(genevents.IoPlatformOrchestratorEnvironmentDeleted),
		"unsubscribed " + string(genevents.IoPlatformOrchestratorEnvironmentDeleted),
		"subscribed " + string(genevents.IoPlatformOrchestratorModuleCatalogueChanged),
	}, projected, "only journal event types are projected, in order")

	_, err = extensionJournalEntries([]*hstandardoutbox.PendingEventMessage{{Subject: string(genevents.IoPlatformOrchestratorEnvironmentDeleted), Payload: []byte(`{"data":{}}`)}})
	require.Error(t, err, "a journal event without an organization is rejected")
}

func TestExtensionResourceRetentionPolicyBounds(t *testing.T) {
	require.NoError(t, ExtensionResourceRetention{Window: 30 * 24 * time.Hour, Batch: 10000}.Validate())
	require.NoError(t, ExtensionResourceRetention{Window: time.Hour, Batch: 1}.Validate())
	for name, policy := range map[string]ExtensionResourceRetention{
		"zero window":     {Window: 0, Batch: 10000},
		"negative window": {Window: -time.Hour, Batch: 10000},
		"below one hour":  {Window: 59 * time.Minute, Batch: 10000},
		"zero batch":      {Window: time.Hour, Batch: 0},
		"oversized batch": {Window: time.Hour, Batch: 100001},
	} {
		require.Error(t, policy.Validate(), name)
	}
}

func TestJournalOrganizationsKeepLockOrderAndEventOrder(t *testing.T) {
	entry := func(org, reason string) extensionResourceJournalEntry {
		return extensionResourceJournalEntry{orgID: org, event: ExtensionResourceEvent{Reason: reason}}
	}
	groups := journalOrganizations([]extensionResourceJournalEntry{
		entry("zeta", "z1"), entry("alpha", "a1"), entry("paused", "p1"),
		entry("zeta", "z2"), entry("alpha", "a2"), entry("paused", "p2"),
	})
	require.Len(t, groups, 3)
	orgs, reasons := []string{}, map[string][]string{}
	for _, group := range groups {
		orgs = append(orgs, group.orgID)
		for _, item := range group.entries {
			reasons[group.orgID] = append(reasons[group.orgID], item.event.Reason)
		}
	}
	require.Equal(t, []string{"alpha", "paused", "zeta"}, orgs, "every writer locks organizations in the same order")
	require.Equal(t, map[string][]string{"alpha": {"a1", "a2"}, "paused": {"p1", "p2"}, "zeta": {"z1", "z2"}}, reasons, "event order is kept within each organization")
}

func TestExtensionResourceStartHonoursRetentionAndGaps(t *testing.T) {
	cursor := func(value int64) *int64 { return &value }
	for name, tc := range map[string]struct {
		after          *int64
		head, horizon  int64
		start          int64
		expired, ahead bool
	}{
		"fresh read starts at the horizon":        {after: nil, head: 9, horizon: 5, start: 5},
		"fresh read of an untouched journal":      {after: nil, head: 9, horizon: 0, start: 0},
		"cursor at the horizon is still complete": {after: cursor(5), head: 9, horizon: 5, start: 5},
		"cursor before the horizon expires":       {after: cursor(4), head: 9, horizon: 5, expired: true},
		"cursor issued before a gap expires":      {after: cursor(9), head: 10, horizon: 10, expired: true},
		"cursor issued after the gap is valid":    {after: cursor(10), head: 12, horizon: 10, start: 10},
		"cursor ahead of the journal is rejected": {after: cursor(13), head: 12, horizon: 0, ahead: true},
		"zero cursor on a pruned journal expires": {after: cursor(0), head: 3, horizon: 1, expired: true},
	} {
		t.Run(name, func(t *testing.T) {
			start, err := extensionResourceStart(tc.after, tc.head, tc.horizon)
			switch {
			case tc.expired:
				require.ErrorIs(t, err, ErrExtensionResourceCursorExpired)
			case tc.ahead:
				_, isBadRequest := IsErrBadRequest(err)
				require.True(t, isBadRequest, "%v", err)
			default:
				require.NoError(t, err)
				require.Equal(t, tc.start, start)
			}
		})
	}
}
