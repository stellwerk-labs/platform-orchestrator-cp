//go:build integration

package model

import (
	"os"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func journalState(t *testing.T, db Databaser, org string) (bool, int64) {
	t.Helper()
	var journaling bool
	var gap int64
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT journaling, gap_cursor FROM extension_resource_event_journal_state WHERE org_id=$1`, org).Scan(&journaling, &gap))
	return journaling, gap
}

func TestExtensionResourceJournalGapAfterSubscriptionReturns(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 2)
	_, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *consumer, nil, 10)
	require.NoError(t, err)
	require.Equal(t, int64(2), cursor)

	// Restart with the plugin host disabled: nothing is journaled meanwhile.
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, false))
	journaling, gap := journalState(t, db, consumer.OrgID)
	require.False(t, journaling)
	require.Zero(t, gap)
	journalEnvironmentEvents(t, db, consumer, 1)
	require.Equal(t, []int64{1, 2}, journalCursors(t, db, consumer.OrgID))

	// Restart with the plugin host enabled: the subscription still exists.
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, true))
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, true), "reconciliation is idempotent")
	journaling, gap = journalState(t, db, consumer.OrgID)
	require.True(t, journaling)
	require.Equal(t, int64(3), gap, "the gap takes its own cursor after every cursor issued before it")

	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *consumer, &cursor, 10)
	require.ErrorIs(t, err, ErrExtensionResourceCursorExpired, "a cursor from before the gap must resynchronise")
	fresh, next, err := ReadExtensionResourceEvents(t.Context(), db, *consumer, nil, 10)
	require.NoError(t, err)
	require.Empty(t, fresh, "a read without a cursor starts after the gap")
	require.Equal(t, gap, next)

	journalEnvironmentEvents(t, db, consumer, 1)
	items, after, err := ReadExtensionResourceEvents(t.Context(), db, *consumer, &next, 10)
	require.NoError(t, err)
	require.Len(t, items, 1, "a cursor issued after the gap stays valid")
	require.Equal(t, int64(4), after)
	_, latestGap := journalState(t, db, consumer.OrgID)
	require.Equal(t, gap, latestGap, "journaled writes after the gap record no further gap")
}

func TestExtensionResourceJournalGapFromAPausedReplica(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 1)
	_, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *consumer, nil, 10)
	require.NoError(t, err)

	// Another replica removes the only subscription: the pause is shared
	// state, so no replica journals the changes either one handles meanwhile.
	var schema string
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	other, err := NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(os.Getenv("PLUGIN_TEST_DATABASE_URL"), schema), WithExtensionResourceJournal())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	unsubscribeFromResourceEvents(t, other, consumer.OrgID, consumer.PluginID)
	journaling, _ := journalState(t, db, consumer.OrgID)
	require.False(t, journaling, "removing the only subscription pauses the journal in the same transaction")
	journalEnvironmentEvents(t, other, consumer, 1)
	journalEnvironmentEvents(t, db, consumer, 1)
	require.Equal(t, []int64{1}, journalCursors(t, db, consumer.OrgID), "nothing is journaled without a subscriber")
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *consumer, nil, 10)
	conflict, ok := IsErrConflict(err)
	require.True(t, ok, "an unsubscribed plugin cannot read the journal: %v", err)
	require.Equal(t, "plugin_not_installed", conflict.Code)

	// The subscription returns through the other replica; this one sees the gap.
	subscribeToResourceEvents(t, other, consumer.OrgID, consumer.PluginID)
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *consumer, &cursor, 10)
	require.ErrorIs(t, err, ErrExtensionResourceCursorExpired)
	journaling, gap := journalState(t, db, consumer.OrgID)
	require.True(t, journaling)
	require.Equal(t, int64(2), gap)

	// Another subscriber keeps the journal running while one plugin leaves.
	subscribeToResourceEvents(t, other, consumer.OrgID, "dev.example.keeper")
	unsubscribeFromResourceEvents(t, other, consumer.OrgID, consumer.PluginID)
	journalEnvironmentEvents(t, db, consumer, 1)
	journaling, sameGap := journalState(t, db, consumer.OrgID)
	require.True(t, journaling)
	require.Equal(t, gap, sameGap)
	require.Equal(t, []int64{1, 3}, journalCursors(t, db, consumer.OrgID))
}

func TestExtensionResourceJournalStateMigrationRefusesDowngradeAfterAPause(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 1)
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, false))
	provider := extensionMigrationProvider(t, db)
	_, err := provider.DownTo(t.Context(), 39)
	require.ErrorContains(t, err, "cannot remove recorded extension journal pauses or gaps")
	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(40), version)
}
