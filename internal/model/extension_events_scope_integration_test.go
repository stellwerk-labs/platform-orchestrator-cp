//go:build integration

package model

import (
	"testing"

	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
)

func journalCursors(t *testing.T, db Databaser, org string) []int64 {
	t.Helper()
	var cursors pq.Int64Array
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT COALESCE(array_agg(cursor ORDER BY cursor),'{}') FROM extension_resource_events WHERE org_id=$1`, org).Scan(&cursors))
	return cursors
}

func TestExtensionResourceJournalIsOffWithoutSubscribers(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	command := externalGrantFixture(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	var before int
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&before))

	// The Core-only path accepts a raw transaction, exactly like a release
	// without the plugin host.
	raw, err := db.(*databaser).DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	envID := grant.Intent.Environments[0].EnvironmentID
	_, err = db.InsertPendingEventMessages(t.Context(), raw, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, grant.OrgID, "core-only", &envID, nil),
	})
	require.NoError(t, err)
	require.NoError(t, raw.Commit())

	var after, journal, heads int
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM pending_event_messages),
		(SELECT count(*) FROM extension_resource_events), (SELECT count(*) FROM extension_resource_event_heads)`).Scan(&after, &journal, &heads))
	require.Equal(t, before+1, after, "the ordinary outbox message is still written")
	require.Zero(t, journal, "fixture and explicit events leave no journal entries")
	require.Zero(t, heads)
}

func TestExtensionResourceJournalRecordsOnlySubscribedOrganizations(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	subscribed := command.OrgID
	resetExtensionEventFixtures(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	envID := grant.Intent.Environments[0].EnvironmentID
	_, err = db.InsertPendingEventMessages(t.Context(), nil, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, subscribed, "subscribed", &envID, nil),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, "unsubscribed-org", "unsubscribed", &envID, nil),
	})
	require.NoError(t, err)
	var orgs pq.StringArray
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT COALESCE(array_agg(DISTINCT org_id),'{}') FROM extension_resource_events`).Scan(&orgs))
	require.Equal(t, []string{subscribed}, []string(orgs))
	require.Equal(t, []int64{1}, journalCursors(t, db, subscribed))
	var outbox int
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outbox))
	require.Equal(t, 2, outbox, "both organizations keep their outbox messages")
}
