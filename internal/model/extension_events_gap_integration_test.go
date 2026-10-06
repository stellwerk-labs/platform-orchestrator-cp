//go:build integration

package model

import (
	"os"
	"sync/atomic"
	"testing"

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
	var subscribed atomic.Bool
	subscribed.Store(true)
	scope := func(string) bool { return subscribed.Load() }
	db := externalOperationDatabaseWith(t, WithExtensionResourceJournal(scope))
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 2)
	_, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *consumer, nil, 10)
	require.NoError(t, err)
	require.Equal(t, int64(2), cursor)

	// Restart with the plugin host disabled: nothing is journaled meanwhile.
	subscribed.Store(false)
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, nil))
	journaling, gap := journalState(t, db, consumer.OrgID)
	require.False(t, journaling)
	require.Zero(t, gap)
	journalEnvironmentEvents(t, db, consumer, 1)
	require.Equal(t, []int64{1, 2}, journalCursors(t, db, consumer.OrgID))

	// Restart with the subscription restored.
	subscribed.Store(true)
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, scope))
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, scope), "reconciliation is idempotent")
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

	// Another replica runs without the subscription and handles a change.
	var schema string
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	paused, err := NewDatabaser(t.Context(), zap.NewNop(), os.Getenv("PLUGIN_TEST_DATABASE_URL")+" search_path="+schema,
		WithExtensionResourceJournal(func(string) bool { return false }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, paused.Close()) })
	journalEnvironmentEvents(t, paused, consumer, 1)
	journaling, _ := journalState(t, db, consumer.OrgID)
	require.False(t, journaling, "skipping an event of a journaled organization records the pause")

	// The subscribed replica marks the gap on the next read, without waiting for a write.
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *consumer, &cursor, 10)
	require.ErrorIs(t, err, ErrExtensionResourceCursorExpired)
	journaling, gap := journalState(t, db, consumer.OrgID)
	require.True(t, journaling)
	require.Equal(t, int64(2), gap)

	// A write by the subscribed replica after another pause records a new gap.
	journalEnvironmentEvents(t, paused, consumer, 1)
	journalEnvironmentEvents(t, db, consumer, 1)
	_, newGap := journalState(t, db, consumer.OrgID)
	require.Equal(t, int64(3), newGap)
	require.Equal(t, []int64{1, 4}, journalCursors(t, db, consumer.OrgID))
}

func TestExtensionResourceJournalStateMigrationRefusesDowngradeAfterAPause(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 1)
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, nil))
	provider := extensionMigrationProvider(t, db)
	_, err := provider.DownTo(t.Context(), 39)
	require.ErrorContains(t, err, "cannot remove recorded extension journal pauses or gaps")
	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(40), version)
}
