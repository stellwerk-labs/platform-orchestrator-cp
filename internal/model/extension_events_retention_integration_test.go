//go:build integration

package model

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/genevents"
)

var testRetention = ExtensionResourceRetention{Window: 30 * 24 * time.Hour, Batch: 1000}

// journalConsumer prepares a further operation whose frozen intent delegates
// resource-events.read, making it a journal consumer.
func journalConsumer(t *testing.T, db Databaser, command ExternalGrantCommand, requestID, digest string) *ExternalOperationGrant {
	t.Helper()
	intent := *command.Intent
	intent.Actions = append(slices.Clone(intent.Actions), extensionResourceEventsRead)
	command.Intent = &intent
	command.OperationID, command.RequestID, command.SourceDigest = uuid.New(), requestID, strings.Repeat(digest, 64)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	return grant
}

func journalEnvironmentEvents(t *testing.T, db Databaser, grant *ExternalOperationGrant, count int) {
	t.Helper()
	envID := grant.Intent.Environments[0].EnvironmentID
	messages := []*hstandardoutbox.PendingEventMessage{}
	for range count {
		messages = append(messages, extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, grant.OrgID, "retained", &envID, nil))
	}
	_, err := db.InsertPendingEventMessages(t.Context(), nil, messages)
	require.NoError(t, err)
}

// ageJournal backdates entries; no API can create an old entry.
func ageJournal(t *testing.T, db Databaser, org string, through int64) {
	t.Helper()
	_, err := db.(*databaser).ExecContext(t.Context(), `UPDATE extension_resource_events SET recorded_at=now()-interval '40 days' WHERE org_id=$1 AND cursor<=$2`, org, through)
	require.NoError(t, err)
}

func prunedThrough(t *testing.T, db Databaser, org string) int64 {
	t.Helper()
	var through int64
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT COALESCE((SELECT pruned_through FROM extension_resource_event_retention WHERE org_id=$1),0)`, org).Scan(&through))
	return through
}

func TestExtensionResourceRetentionNeverPassesAnActiveConsumer(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	first := journalConsumer(t, db, command, "consumer-first", "e")
	journalEnvironmentEvents(t, db, first, 6)
	ageJournal(t, db, first.OrgID, 5)

	pruned, err := PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Zero(t, pruned, "a consumer without an acknowledgement holds all history")

	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *first, ref.Ref(int64(4)), 10)
	require.NoError(t, err)
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(3), pruned)
	require.Equal(t, []int64{4, 5, 6}, journalCursors(t, db, first.OrgID), "the acknowledged cursor itself is retained")
	require.Equal(t, int64(3), prunedThrough(t, db, first.OrgID))

	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *first, ref.Ref(int64(6)), 10)
	require.NoError(t, err)
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(2), pruned)
	require.Equal(t, []int64{6}, journalCursors(t, db, first.OrgID), "entries younger than the window are retained")

	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *first, ref.Ref(int64(2)), 10)
	require.ErrorIs(t, err, ErrExtensionResourceCursorExpired, "a cursor before pruned history never skips silently")
	items, next, err := ReadExtensionResourceEvents(t.Context(), db, *first, ref.Ref(int64(5)), 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, int64(6), next)
	fresh, freshNext, err := ReadExtensionResourceEvents(t.Context(), db, *first, nil, 10)
	require.NoError(t, err)
	require.Equal(t, items, fresh, "without a cursor reading starts at the oldest retained entry")
	require.Equal(t, next, freshNext)

	second := journalConsumer(t, db, command, "consumer-second", "f")
	journalEnvironmentEvents(t, db, first, 3)
	ageJournal(t, db, first.OrgID, 9)
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *first, ref.Ref(int64(9)), 10)
	require.NoError(t, err)
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Zero(t, pruned, "the lowest acknowledgement across consumers bounds retention")
	_, secondCursor, err := ReadExtensionResourceEvents(t.Context(), db, *second, nil, 10)
	require.NoError(t, err)
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *second, &secondCursor, 10)
	require.NoError(t, err)
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(3), pruned)
	require.Equal(t, []int64{9}, journalCursors(t, db, first.OrgID))
}

func TestExtensionResourceRetentionHonoursReleaseWindowAndUninstall(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 2)
	ageJournal(t, db, consumer.OrgID, 2)

	// Another subscriber keeps the journal running while the consumer's
	// plugin is not enabled for the organization.
	subscribeToResourceEvents(t, db, consumer.OrgID, "dev.example.keeper")
	unsubscribeFromResourceEvents(t, db, consumer.OrgID, consumer.PluginID)
	pruned, err := PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(2), pruned, "operations of a plugin that is no longer installed are not consumers")
	subscribeToResourceEvents(t, db, consumer.OrgID, consumer.PluginID)

	journalEnvironmentEvents(t, db, consumer, 2)
	ageJournal(t, db, consumer.OrgID, 4)
	release := func(at string) {
		// Only a database fixture can backdate the release time.
		_, err := db.(*databaser).ExecContext(t.Context(), `UPDATE external_operation_grants SET state='released', updated_at=`+at+` WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3`,
			consumer.OrgID, consumer.PluginID, consumer.OperationID)
		require.NoError(t, err)
	}
	release("now()")
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Zero(t, pruned, "a recently released operation keeps reconciling for one retention window")
	release("now()-interval '40 days'")
	pruned, err = PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(2), pruned)
	require.Empty(t, journalCursors(t, db, consumer.OrgID))
	require.Equal(t, int64(4), prunedThrough(t, db, consumer.OrgID))
}

func TestExtensionResourceRetentionIsBatchedAndSkipsLockedOrganizations(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 5)
	ageJournal(t, db, consumer.OrgID, 5)
	subscribeToResourceEvents(t, db, consumer.OrgID, "dev.example.keeper")
	unsubscribeFromResourceEvents(t, db, consumer.OrgID, consumer.PluginID)

	// Another replica holding the organization's retention lock wins.
	holder, err := db.(*databaser).DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 4))`, "extension-resource-retention:"+consumer.OrgID)
	require.NoError(t, err)
	pruned, err := PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Zero(t, pruned)
	require.NoError(t, holder.Rollback())

	batched := ExtensionResourceRetention{Window: testRetention.Window, Batch: 2}
	for _, expected := range [][]int64{{3, 4, 5}, {5}, {}} {
		_, err = PruneExtensionResourceJournal(t.Context(), db, batched, time.Now())
		require.NoError(t, err)
		require.Equal(t, expected, journalCursors(t, db, consumer.OrgID))
	}
	require.Equal(t, int64(5), prunedThrough(t, db, consumer.OrgID))
}

func TestExtensionResourceRetentionMigrationRefusesDowngradeAfterPruning(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	consumer := journalConsumer(t, db, command, "consumer", "e")
	journalEnvironmentEvents(t, db, consumer, 1)
	ageJournal(t, db, consumer.OrgID, 1)
	subscribeToResourceEvents(t, db, consumer.OrgID, "dev.example.keeper")
	unsubscribeFromResourceEvents(t, db, consumer.OrgID, consumer.PluginID)
	_, err := PruneExtensionResourceJournal(t.Context(), db, testRetention, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(1), prunedThrough(t, db, consumer.OrgID))
	provider := extensionMigrationProvider(t, db)
	initialVersion, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	err = exerciseSQLMigrationDown(t, db, 39, false)
	require.ErrorContains(t, err, "cannot remove extension event retention evidence after pruning")
	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, initialVersion, version)
}
