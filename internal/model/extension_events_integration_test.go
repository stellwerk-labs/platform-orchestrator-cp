//go:build integration

package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func extensionEventMessage(t *testing.T, subject genevents.EventType, orgID, reason string, envID, moduleID *uuid.UUID) *hstandardoutbox.PendingEventMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"specversion": "1.0",
		"type":        subject,
		"time":        time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC),
		"data": map[string]any{
			"org_id":           orgID,
			"event_type":       "published",
			"catalogue_status": "archived",
			"lifecycle_status": "proposed",
			"actor":            uuid.MustParse("11111111-1111-4111-8111-111111111111"),
			"environment_uuid": envID,
			"module_uuid":      moduleID,
			"reason":           reason,
			// These values must be replaced by the trusted envelope/subject.
			"event_id":    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
			"type":        "caller.controlled",
			"occurred_at": time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)
	return &hstandardoutbox.PendingEventMessage{Subject: string(subject), Payload: payload}
}

func resetExtensionEventFixtures(t *testing.T, db Databaser) {
	t.Helper()
	_, err := db.(*databaser).ExecContext(t.Context(), `TRUNCATE extension_resource_events, extension_resource_event_heads, extension_resource_event_consumers, extension_resource_event_retention, extension_resource_event_journal_state, extension_published_events, pending_event_messages`)
	require.NoError(t, err)
	require.NoError(t, ReconcileExtensionResourceJournal(t.Context(), db, true), "subscribed organizations journal again")
}

func extensionMigrationProvider(t *testing.T, db Databaser) *goose.Provider {
	t.Helper()
	source, err := fs.Sub(embedMigrations, "migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectPostgres, db.(*databaser).DB, source,
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(goose.NewGoMigration(11,
			&goose.GoFunc{RunTx: hstandardoutbox.MigrateUp01},
			&goose.GoFunc{RunTx: hstandardoutbox.MigrateDown01})))
	require.NoError(t, err)
	return provider
}

func TestExtensionResourceJournalIsAtomicWithOutboxAndProjectsTrustedFields(t *testing.T) {
	db := externalOperationDatabase(t)
	grantCommand := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	moduleID := grantCommand.Intent.Modules[0]
	message := extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished,
		grantCommand.OrgID, "atomic projection", nil, &moduleID)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), tx, []*hstandardoutbox.PendingEventMessage{message})
	require.NoError(t, err)
	var journalRows, outboxRows int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_resource_events`).Scan(&journalRows))
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outboxRows))
	require.Zero(t, journalRows, "journal projection is deferred until the transaction has finished taking resource locks")
	require.Equal(t, 1, outboxRows)
	require.NoError(t, tx.Rollback())
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_resource_events`).Scan(&journalRows))
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages`).Scan(&outboxRows))
	require.Zero(t, journalRows)
	require.Zero(t, outboxRows)
	var heads int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_resource_event_heads`).Scan(&heads))
	require.Zero(t, heads)

	message = extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished,
		grantCommand.OrgID, "atomic projection", nil, &moduleID)
	_, err = db.InsertPendingEventMessages(t.Context(), nil, []*hstandardoutbox.PendingEventMessage{message})
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), nil, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleCatalogueChanged,
			grantCommand.OrgID, "catalogue projection", nil, &moduleID),
	})
	require.NoError(t, err)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, grantCommand)
	require.NoError(t, err)
	items, next, err := ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 20)
	require.NoError(t, err)
	require.Equal(t, int64(2), next)
	require.Len(t, items, 2)
	require.NotEqual(t, uuid.MustParse("22222222-2222-4222-8222-222222222222"), items[0].EventID)
	require.Equal(t, string(genevents.IoPlatformOrchestratorModuleVersionPublished), items[0].Type)
	require.Equal(t, time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC), items[0].OccurredAt)
	require.Equal(t, uuid.MustParse("11111111-1111-4111-8111-111111111111"), *items[0].ActorID)
	require.Equal(t, "published", items[0].Transition)
	require.Equal(t, "atomic projection", items[0].Reason)
	require.Equal(t, moduleID, *items[0].ModuleUUID)
	require.Equal(t, "proposed", items[0].ToStatus, "published lifecycle_status must populate the neutral status field")
	require.Equal(t, "archived", items[1].ToStatus, "catalogue_status must populate the neutral status field")
}

func TestExtensionResourceJournalScopesPaginationReplayAndReconnect(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	inEnv := grant.Intent.Environments[0].EnvironmentID
	inModule := grant.Intent.Modules[0]
	otherEnv, otherModule := uuid.New(), uuid.New()
	messages := []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, grant.OrgID, "visible-env", &inEnv, nil),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorEnvironmentDeleted, grant.OrgID, "hidden-env", &otherEnv, nil),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, grant.OrgID, "visible-module", nil, &inModule),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionLifecycleChanged, grant.OrgID, "visible-both", &inEnv, &inModule),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleCatalogueChanged, grant.OrgID, "hidden-unscoped", nil, nil),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionLifecycleChanged, grant.OrgID, "hidden-cross-scope", &inEnv, &otherModule),
	}
	_, err = db.InsertPendingEventMessages(t.Context(), nil, messages)
	require.NoError(t, err)

	first, next, err := ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"visible-env", "visible-module"}, []string{first[0].Reason, first[1].Reason})
	require.Equal(t, int64(3), next)
	replayed, replayNext, err := ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 2)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	require.Equal(t, next, replayNext)
	second, finalCursor, err := ReadExtensionResourceEvents(t.Context(), db, *grant, &next, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, "visible-both", second[0].Reason)
	require.Equal(t, int64(6), finalCursor, "the cursor advances across filtered events without exposing them")
	empty, stableCursor, err := ReadExtensionResourceEvents(t.Context(), db, *grant, &finalCursor, 2)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.Equal(t, finalCursor, stableCursor)
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *grant, ref.Ref(finalCursor+1), 2)
	require.ErrorContains(t, err, "ahead")
	_, _, err = ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 201)
	require.ErrorContains(t, err, "invalid")

	var schema string
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	reconnected, err := NewDatabaser(t.Context(), zap.NewNop(), pgtest.WithSearchPath(os.Getenv("PLUGIN_TEST_DATABASE_URL"), schema), WithExtensionResourceJournal())
	require.NoError(t, err)
	defer func() { require.NoError(t, reconnected.Close()) }()
	afterReconnect, reconnectCursor, err := ReadExtensionResourceEvents(t.Context(), reconnected, *grant, &next, 2)
	require.NoError(t, err)
	require.Equal(t, second, afterReconnect)
	require.Equal(t, finalCursor, reconnectCursor)
}

func TestExtensionResourceCursorFollowsCommitOrderWithoutSkippingOpenTransactions(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	moduleID := grant.Intent.Modules[0]

	firstTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), firstTx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, grant.OrgID, "first-open", nil, &moduleID),
	})
	require.NoError(t, err)
	secondTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), secondTx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, grant.OrgID, "second-committed", nil, &moduleID),
	})
	require.NoError(t, err, "event collection must not wait on another transaction's journal head")
	before, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 20)
	require.NoError(t, err)
	require.Empty(t, before)
	require.Zero(t, cursor)
	require.NoError(t, secondTx.Commit())
	firstPage, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *grant, nil, 20)
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	require.Equal(t, "second-committed", firstPage[0].Reason)
	require.Equal(t, int64(1), cursor)
	require.NoError(t, firstTx.Commit())
	secondPage, cursor, err := ReadExtensionResourceEvents(t.Context(), db, *grant, &cursor, 20)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	require.Equal(t, "first-open", secondPage[0].Reason)
	require.Equal(t, int64(2), cursor)
}

func TestExtensionResourceJournalRejectsRawTransactionsAndRollsBackFlushFailures(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	moduleID := command.Intent.Modules[0]

	rawTx, err := db.(*databaser).DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), rawTx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, command.OrgID, "raw", nil, &moduleID),
	})
	require.ErrorContains(t, err, "require a Databaser transaction")
	require.NoError(t, rawTx.Rollback())

	// Occupy the cursor the next journal entry takes, so the flush fails.
	_, err = db.(*databaser).ExecContext(t.Context(), `INSERT INTO extension_resource_events (org_id,cursor,event_id,event) VALUES ($1,1,$2,'{}')`, command.OrgID, uuid.New())
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), tx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, command.OrgID, "flush-failure", nil, &moduleID),
	})
	require.NoError(t, err, "the outbox write and immutable projection are prepared before commit")
	require.Error(t, tx.Commit(), "a failing journal write must fail the commit closed")
	var outboxRows int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages WHERE payload::text LIKE '%flush-failure%'`).Scan(&outboxRows))
	require.Zero(t, outboxRows, "a failed projection flush must roll back its transactional outbox write")
}

func TestExtensionResourceJournalSerializesRecognizedInsertWithCommit(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	moduleID := command.Intent.Modules[0]

	blocker, err := db.(*databaser).DB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = blocker.ExecContext(t.Context(), `LOCK TABLE pending_event_messages IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	journalTx := tx.(*journalTransaction)
	message := extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, command.OrgID, "serialized", nil, &moduleID)
	inserted := make(chan error, 1)
	go func() {
		_, insertErr := db.InsertPendingEventMessages(t.Context(), tx, []*hstandardoutbox.PendingEventMessage{message})
		inserted <- insertErr
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for journalTx.mu.TryLock() {
		journalTx.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("recognized insert never entered its transaction-critical section")
		case <-time.After(time.Millisecond):
		}
	}
	committed := make(chan error, 1)
	go func() { committed <- tx.Commit() }()
	select {
	case commitErr := <-committed:
		require.NoError(t, commitErr)
		t.Fatal("commit interleaved with the blocked recognized outbox insert")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, blocker.Rollback())
	select {
	case insertErr := <-inserted:
		require.NoError(t, insertErr)
	case <-ctx.Done():
		t.Fatal("recognized outbox insert did not resume")
	}
	select {
	case commitErr := <-committed:
		require.NoError(t, commitErr)
	case <-ctx.Done():
		t.Fatal("commit did not resume after the complete recognized insert was queued")
	}
	var journalRows, outboxRows int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_resource_events WHERE org_id=$1`, command.OrgID).Scan(&journalRows))
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages WHERE payload::text LIKE '%serialized%'`).Scan(&outboxRows))
	require.Equal(t, 1, journalRows)
	require.Equal(t, 1, outboxRows)
}

func TestExtensionResourceJournalSortsOrganizationHeadLocks(t *testing.T) {
	db := externalOperationDatabase(t)
	resetExtensionEventFixtures(t, db)
	actor := uuid.New()
	for _, orgID := range []string{"journal-lock-a", "journal-lock-b"} {
		_, err := db.CreateOrg(t.Context(), nil, &Org{Id: orgID, CreatedBy: actor, CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
		require.NoError(t, err)
		subscribeToResourceEvents(t, db, orgID, "dev.example."+orgID)
	}
	moduleID := uuid.New()
	firstTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), firstTx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, "journal-lock-a", "first-a", nil, &moduleID),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, "journal-lock-b", "first-b", nil, &moduleID),
	})
	require.NoError(t, err)
	secondTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.InsertPendingEventMessages(t.Context(), secondTx, []*hstandardoutbox.PendingEventMessage{
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, "journal-lock-b", "second-b", nil, &moduleID),
		extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, "journal-lock-a", "second-a", nil, &moduleID),
	})
	require.NoError(t, err)

	committed := make(chan error, 2)
	go func() { committed <- firstTx.Commit() }()
	go func() { committed <- secondTx.Commit() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		select {
		case commitErr := <-committed:
			require.NoError(t, commitErr)
		case <-ctx.Done():
			t.Fatal("opposite organization event order deadlocked during commit")
		}
	}
	for _, orgID := range []string{"journal-lock-a", "journal-lock-b"} {
		var cursor, events int64
		require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT cursor FROM extension_resource_event_heads WHERE org_id=$1`, orgID).Scan(&cursor))
		require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_resource_events WHERE org_id=$1`, orgID).Scan(&events))
		require.Equal(t, int64(2), cursor)
		require.Equal(t, int64(2), events)
	}
}

func TestDeferredJournalAvoidsBulkPinAndSinglePinLockInversion(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	firstScope := grant.Intent.Environments[0]
	firstEnvironment, err := db.GetEnvironmentByUuid(t.Context(), nil, grant.OrgID, firstScope.EnvironmentID, GetModeDefault)
	require.NoError(t, err)
	secondEnvironment, err := db.CreateEnvironment(t.Context(), nil, &Environment{
		OrgId: grant.OrgID, OrgUuid: firstEnvironment.OrgUuid, ProjectId: firstEnvironment.ProjectId,
		ProjectUuid: firstEnvironment.ProjectUuid, Id: "second", EnvTypeId: firstEnvironment.EnvTypeId,
		EnvTypeUuid: firstEnvironment.EnvTypeUuid, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Status: EnvironmentStatusActive,
	})
	require.NoError(t, err)
	firstModule, secondModule := grant.Intent.Modules[0], grant.Intent.Modules[1]

	bulkTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateEnvironmentModuleVersionPin(t.Context(), bulkTx, grant.OrgID,
		firstEnvironment.ProjectUuid, firstEnvironment.ProjectId, firstEnvironment.Uuid, firstEnvironment.Id,
		firstModule, *grant.BaselineDefaults[firstModule.String()], grant.ActorID, "user", "bulk first", nil, false)
	require.NoError(t, err)
	singleTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, lockExternalDispatchEnvironment(t.Context(), singleTx, secondEnvironment.Uuid))

	type pinResult struct {
		err error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	singleDone := make(chan pinResult, 1)
	go func() {
		_, createErr := db.CreateEnvironmentModuleVersionPin(ctx, singleTx, grant.OrgID,
			secondEnvironment.ProjectUuid, secondEnvironment.ProjectId, secondEnvironment.Uuid, secondEnvironment.Id,
			secondModule, *grant.BaselineDefaults[secondModule.String()], grant.ActorID, "user", "single", nil, false)
		singleDone <- pinResult{err: createErr}
	}()
	bulkDone := make(chan pinResult, 1)
	go func() {
		_, createErr := db.CreateEnvironmentModuleVersionPin(ctx, bulkTx, grant.OrgID,
			secondEnvironment.ProjectUuid, secondEnvironment.ProjectId, secondEnvironment.Uuid, secondEnvironment.Id,
			firstModule, *grant.BaselineDefaults[firstModule.String()], grant.ActorID, "user", "bulk second", nil, false)
		bulkDone <- pinResult{err: createErr}
	}()
	select {
	case result := <-singleDone:
		require.NoError(t, result.err, "a single Pin must finish its resource work without waiting for the bulk transaction's journal head")
	case <-ctx.Done():
		t.Fatal("single Pin remained blocked by journal/resource lock inversion")
	}
	require.NoError(t, singleTx.Commit())
	select {
	case result := <-bulkDone:
		require.NoError(t, result.err)
	case <-ctx.Done():
		t.Fatal("bulk Pin did not resume after the single Environment lock committed")
	}
	require.NoError(t, bulkTx.Commit())
}

func TestDeferredJournalAvoidsBulkModuleAndSingleCatalogueLockInversion(t *testing.T) {
	db := externalOperationDatabase(t)
	actor := uuid.New()
	orgID, targets, _ := externalOperationModules(t, db, actor)
	resetExtensionEventFixtures(t, db)
	first, second := targets[0], targets[1]
	secondModule, err := db.GetModuleCatalogue(t.Context(), nil, orgID, second.ModuleUUID.String(), GetModeDefault)
	require.NoError(t, err)

	bulkTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.TransitionCoreModuleVersion(t.Context(), bulkTx, orgID, first.ModuleUUID.String(), first.VersionUUID.String(),
		"default", first.ExpectedResourceVersion, actor, "bulk first", nil)
	require.NoError(t, err)
	singleTx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.GetModuleCatalogue(t.Context(), singleTx, orgID, second.ModuleUUID.String(), GetModeForUpdate)
	require.NoError(t, err)

	type moduleResult struct {
		err error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	singleDone := make(chan moduleResult, 1)
	go func() {
		_, archiveErr := db.SetModuleCatalogueStatus(ctx, singleTx, orgID, second.ModuleUUID.String(),
			moduleversions.CatalogueArchived, actor, "single archive", secondModule.ResourceVersion)
		singleDone <- moduleResult{err: archiveErr}
	}()
	bulkDone := make(chan moduleResult, 1)
	go func() {
		_, transitionErr := db.TransitionCoreModuleVersion(ctx, bulkTx, orgID, second.ModuleUUID.String(), second.VersionUUID.String(),
			"default", second.ExpectedResourceVersion, actor, "bulk second", nil)
		bulkDone <- moduleResult{err: transitionErr}
	}()
	select {
	case result := <-singleDone:
		require.NoError(t, result.err, "single catalogue change must not wait for a bulk transaction's journal head")
	case <-ctx.Done():
		t.Fatal("single Module catalogue change remained blocked by journal/resource lock inversion")
	}
	require.NoError(t, singleTx.Commit())
	select {
	case result := <-bulkDone:
		require.ErrorContains(t, result.err, "archived modules", "bulk work must resume and observe the committed catalogue change")
	case <-ctx.Done():
		t.Fatal("bulk Module transition did not resume after the single Module lock committed")
	}
	require.NoError(t, bulkTx.Rollback())
}

func TestExtensionPublicationReceiptsNamespaceAndFence(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	resetExtensionEventFixtures(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	event := ExtensionPublishedEvent{
		EventID: uuid.New(), GrantFence: grant.Revision, Type: "plugin." + grant.PluginID + ".rollout-completed",
		OccurredAt: time.Date(2026, 9, 28, 18, 30, 0, 0, time.UTC), Data: map[string]any{"status": "succeeded"},
	}
	require.NoError(t, PublishExtensionEvent(t.Context(), db, *grant, event))
	require.NoError(t, PublishExtensionEvent(t.Context(), db, *grant, event), "lost acknowledgements must not duplicate publication")
	var receipts, messages int
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_published_events WHERE event_id=$1`, event.EventID).Scan(&receipts))
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages WHERE routing_key=$1`, event.Type).Scan(&messages))
	require.Equal(t, 1, receipts)
	require.Equal(t, 1, messages)
	var payload []byte
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT payload FROM pending_event_messages WHERE routing_key=$1`, event.Type).Scan(&payload))
	var envelope struct {
		SpecVersion string         `json:"specversion"`
		ID          uuid.UUID      `json:"id"`
		Source      string         `json:"source"`
		Type        string         `json:"type"`
		OrgID       string         `json:"org_id"`
		OperationID uuid.UUID      `json:"operation_id"`
		Data        map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(payload, &envelope))
	require.Equal(t, "1.0", envelope.SpecVersion)
	require.Equal(t, event.EventID, envelope.ID)
	require.Equal(t, "plugin:"+grant.PluginID, envelope.Source)
	require.Equal(t, event.Type, envelope.Type)
	require.Equal(t, grant.OrgID, envelope.OrgID)
	require.Equal(t, grant.OperationID, envelope.OperationID)
	require.Equal(t, event.Data, envelope.Data)

	changed := event
	changed.Data = map[string]any{"status": "failed"}
	err = PublishExtensionEvent(t.Context(), db, *grant, changed)
	require.ErrorIs(t, err, ErrExtensionEventReused)
	stale := event
	stale.EventID, stale.GrantFence = uuid.New(), grant.Revision+1
	err = PublishExtensionEvent(t.Context(), db, *grant, stale)
	require.ErrorIs(t, err, ErrExtensionEventFence)
	for _, eventType := range []string{
		"io.platform-orchestrator.module.version.published",
		"plugin." + grant.PluginID,
		"plugin." + grant.PluginID + ".",
		"plugin." + grant.PluginID + ".child.changed",
		"plugin." + grant.PluginID + ".rollout..completed",
		"plugin." + grant.PluginID + ".1rollout.completed",
		"plugin." + grant.PluginID + ".rollout.completed!",
		"plugin." + grant.PluginID + ".rollout\ncompleted",
		"plugin." + grant.PluginID + ".bad/type",
		"plugin.dev.example.other.rollout.completed",
	} {
		unauthorized := event
		unauthorized.EventID, unauthorized.Type = uuid.New(), eventType
		require.Error(t, PublishExtensionEvent(t.Context(), db, *grant, unauthorized), eventType)
	}
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM extension_published_events`).Scan(&receipts))
	require.Equal(t, 1, receipts)
	require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_event_messages WHERE routing_key LIKE 'plugin.%'`).Scan(&messages))
	require.Equal(t, 1, messages)
}

func TestExtensionEventMigrationDownGuardAndEmptyRoundTrip(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		db := externalOperationDatabase(t)
		provider := extensionMigrationProvider(t, db)
		initialVersion, err := provider.GetDBVersion(t.Context())
		require.NoError(t, err)
		_, err = provider.DownTo(t.Context(), 36)
		require.NoError(t, err)
		version, err := provider.GetDBVersion(t.Context())
		require.NoError(t, err)
		require.Equal(t, int64(36), version)
		var table sql.NullString
		require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT to_regclass('extension_resource_events')::text`).Scan(&table))
		require.False(t, table.Valid)
		_, err = provider.Up(t.Context())
		require.NoError(t, err)
		version, err = provider.GetDBVersion(t.Context())
		require.NoError(t, err)
		require.Equal(t, initialVersion, version)
		require.NoError(t, db.(*databaser).DB.QueryRowContext(t.Context(), `SELECT to_regclass('extension_resource_events')::text`).Scan(&table))
		require.True(t, table.Valid)
	})

	for _, retained := range []string{"resource journal", "publication receipt"} {
		t.Run(retained, func(t *testing.T) {
			db := externalOperationDatabase(t)
			command := externalGrantFixture(t, db)
			resetExtensionEventFixtures(t, db)
			grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
			require.NoError(t, err)
			if retained == "resource journal" {
				moduleID := grant.Intent.Modules[0]
				_, err = db.InsertPendingEventMessages(t.Context(), nil, []*hstandardoutbox.PendingEventMessage{
					extensionEventMessage(t, genevents.IoPlatformOrchestratorModuleVersionPublished, grant.OrgID, "retained", nil, &moduleID),
				})
			} else {
				err = PublishExtensionEvent(t.Context(), db, *grant, ExtensionPublishedEvent{
					EventID: uuid.New(), GrantFence: grant.Revision, Type: "plugin." + grant.PluginID + ".retained",
					OccurredAt: time.Now().UTC(), Data: map[string]any{"retained": true},
				})
			}
			require.NoError(t, err)
			provider := extensionMigrationProvider(t, db)
			_, err = provider.DownTo(t.Context(), 36)
			require.ErrorContains(t, err, "cannot discard extension event history")
			version, versionErr := provider.GetDBVersion(t.Context())
			require.NoError(t, versionErr)
			require.Equal(t, int64(37), version)
		})
	}
}
