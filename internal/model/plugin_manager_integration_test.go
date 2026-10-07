//go:build integration

package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func pluginManagerRevision(t *testing.T, db Databaser) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT revision FROM plugin_manager_state`).Scan(&revision))
	return revision
}

func pluginManagerAuditCount(t *testing.T, db Databaser) int {
	t.Helper()
	var count int
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT count(*) FROM plugin_manager_audit`).Scan(&count))
	return count
}

// markerCommand records one change named marker, or nothing when marker is
// empty, and responds with the marker and the revision.
func markerCommand(t *testing.T, db Databaser, key string, revision int64, marker string) (json.RawMessage, error) {
	t.Helper()
	return ExecutePluginManagerCommand(t.Context(), db, PluginManagerCommand{Actor: fixtureOperator, IdempotencyKey: key, Fingerprint: "marker:" + marker, ExpectedRevision: revision, Reason: "Record " + marker, RequestID: "request-" + key},
		func(context.Context, Tx) (*PluginManagerChange, error) {
			if marker == "" {
				return nil, nil
			}
			return &PluginManagerChange{Action: "marker.record", OrgID: "acme", Before: map[string]string{"marker": "none"}, After: map[string]string{"marker": marker}}, nil
		},
		func(_ context.Context, _ Tx, revision int64) (any, error) {
			return map[string]any{"marker": marker, "revision": revision}, nil
		})
}

func TestPluginManagerCommandsAreIdempotentAndRevisioned(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	first, err := markerCommand(t, db, "key-one", 0, "one")
	require.NoError(t, err)
	require.JSONEq(t, `{"marker":"one","revision":1}`, string(first))

	replay, err := markerCommand(t, db, "key-one", 0, "one")
	require.NoError(t, err)
	require.JSONEq(t, string(first), string(replay), "a replay returns the stored response, although the revision moved")
	require.Equal(t, int64(1), pluginManagerRevision(t, db))
	require.Equal(t, 1, pluginManagerAuditCount(t, db), "a replay is no change")

	_, err = markerCommand(t, db, "key-one", 0, "other")
	requireConflictCode(t, err, "plugin_manager_idempotency_conflict")
	_, err = markerCommand(t, db, "key-two", 0, "two")
	requireConflictCode(t, err, "plugin_manager_revision_conflict")
	require.Equal(t, int64(1), pluginManagerRevision(t, db))
	require.Equal(t, 1, pluginManagerAuditCount(t, db), "a refused command changes nothing")

	unchanged, err := markerCommand(t, db, "key-three", 1, "")
	require.NoError(t, err)
	require.JSONEq(t, `{"marker":"","revision":1}`, string(unchanged), "a command whose state already holds keeps the revision")
	require.Equal(t, 1, pluginManagerAuditCount(t, db), "and writes no audit event")

	for _, command := range []PluginManagerCommand{
		{IdempotencyKey: "k", Fingerprint: "f", Reason: "r"},
		{Actor: fixtureOperator, Fingerprint: "f", Reason: "r"},
		{Actor: fixtureOperator, IdempotencyKey: "k", Fingerprint: "f", Reason: " "},
		{Actor: fixtureOperator, IdempotencyKey: "k", Fingerprint: "f", Reason: strings.Repeat("r", 1001)},
		{Actor: fixtureOperator, IdempotencyKey: "k", Fingerprint: "f", Reason: "r", ExpectedRevision: -1},
	} {
		_, err := ExecutePluginManagerCommand(t.Context(), db, command, nil, nil)
		bad, ok := IsErrBadRequest(err)
		require.True(t, ok, "%+v: %v", command, err)
		require.Equal(t, "plugin_manager_request_invalid", bad.Code)
	}
}

func TestPluginManagerSerialisesConcurrentWriters(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Go(func() {
			_, err := markerCommand(t, db, fmt.Sprintf("writer-%d", i), 0, fmt.Sprintf("writer-%d", i))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		requireConflictCode(t, err, "plugin_manager_revision_conflict")
	}
	require.Equal(t, 1, succeeded, "writers deciding on the same revision serialise; exactly one wins")
	require.Equal(t, int64(1), pluginManagerRevision(t, db))
	require.Equal(t, 1, pluginManagerAuditCount(t, db))
}

func TestPluginManagerAuditIsAppendOnly(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	_, err := markerCommand(t, db, "audited", 0, "audited")
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	events, err := ListPluginManagerAudit(t.Context(), tx, 0, 10)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, int64(1), event.Revision)
	require.Equal(t, fixtureOperator, event.Actor)
	require.Equal(t, "marker.record", event.Action)
	require.Equal(t, "acme", event.OrgID)
	require.Equal(t, "Record audited", event.Reason)
	require.Equal(t, "request-audited", event.RequestID)
	require.Len(t, event.IdempotencyKeyID, 64, "the key is recorded only as its SHA-256")
	require.NotContains(t, event.IdempotencyKeyID, "audited")
	require.JSONEq(t, `{"marker":"none"}`, string(event.Before))
	require.JSONEq(t, `{"marker":"audited"}`, string(event.After))
	require.WithinDuration(t, time.Now(), event.OccurredAt, time.Minute)

	for _, statement := range []string{
		`UPDATE plugin_manager_audit SET reason='rewritten'`,
		`DELETE FROM plugin_manager_audit`,
		`TRUNCATE plugin_manager_audit`,
	} {
		_, err := db.(*databaser).ExecContext(t.Context(), statement)
		require.ErrorContains(t, err, "plugin_manager_audit is append-only", statement)
	}
	require.Equal(t, 1, pluginManagerAuditCount(t, db))
}

func TestPluginManagerMigrationRefusesDowngradeWithAudit(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	_, err := markerCommand(t, db, "audited", 0, "audited")
	require.NoError(t, err)
	provider := extensionMigrationProvider(t, db)
	_, err = provider.DownTo(t.Context(), 40)
	require.ErrorContains(t, err, "cannot remove recorded Plugin Manager state and audit")
	version, err := provider.GetDBVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(41), version)
}

func TestRequirePluginEnablementFollowsTheDatabase(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	org, _, _ := externalOperationModules(t, db, uuid.New())
	installation := subscribeToResourceEvents(t, db, org, "dev.example.effects")
	use := PluginUse{OrgID: org, PluginID: installation.PluginID, InstallationID: installation.InstallationID.String(), PackageSHA256: installation.PackageSHA256}
	check := func(use PluginUse) error {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		return RequirePluginEnablement(t.Context(), tx, use)
	}
	setState := func(table, state string) {
		_, err := db.(*databaser).ExecContext(t.Context(), `UPDATE `+table+` SET state=$1 WHERE installation_uuid=$2`, state, installation.InstallationID)
		require.NoError(t, err)
	}
	require.NoError(t, check(use))

	stale := use
	stale.PackageSHA256 = strings.Repeat("0", 64)
	requireConflictCode(t, check(stale), "plugin_not_installed")
	stale = use
	stale.InstallationID = uuid.NewString()
	requireConflictCode(t, check(stale), "plugin_not_installed")
	foreign := use
	foreign.OrgID = "another-org"
	requireConflictCode(t, check(foreign), "plugin_not_installed")

	safety := use
	safety.Safety = true
	setState("plugin_enablements", PluginEnablementDisabled)
	requireConflictCode(t, check(use), "plugin_not_enabled")
	require.NoError(t, check(safety), "safety work continues for a disabled organization")
	setState("plugin_enablements", PluginEnablementEnabled)
	setState("plugin_installations", PluginInstallationDisabled)
	requireConflictCode(t, check(use), "plugin_not_enabled")
	require.NoError(t, check(safety), "safety work continues for a disabled installation")
	setState("plugin_installations", PluginInstallationActive)

	// An open effect holds the enablement: a concurrent disable waits for its
	// commit, so the effect can never commit after the disable.
	effect, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, RequirePluginEnablement(t.Context(), effect, use))
	disabled := make(chan error, 1)
	go func() {
		_, err := db.(*databaser).ExecContext(context.Background(), `UPDATE plugin_enablements SET state='disabled' WHERE installation_uuid=$1`, installation.InstallationID)
		disabled <- err
	}()
	select {
	case err := <-disabled:
		t.Fatalf("the disable passed an open effect transaction: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, effect.Commit())
	require.NoError(t, <-disabled)
	requireConflictCode(t, check(use), "plugin_not_enabled")

	unsubscribeFromResourceEvents(t, db, org, installation.PluginID)
	requireConflictCode(t, check(safety), "plugin_not_installed")
}

func TestResourceEventSubscribersComeFromInstalledServiceGrants(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	actor := uuid.New()
	orgs := []string{}
	for range 3 {
		org, _, _ := externalOperationModules(t, db, actor)
		orgs = append(orgs, org)
	}
	subscribers := func(org string) []string {
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		plugins, err := resourceEventSubscribers(t.Context(), tx, org)
		require.NoError(t, err)
		return plugins
	}
	subscribeToResourceEvents(t, db, orgs[1], "dev.example.watcher")
	subscribeToResourceEvents(t, db, orgs[1], "dev.example.second")
	// An installation without the resource event read grant is no subscriber.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	installation, err := fixtureInstallation(t, tx, "dev.example.deployer")
	require.NoError(t, err)
	require.NoError(t, ReplacePluginRegistrations(t.Context(), tx, installation, []PluginRegistration{{Kind: PluginRegistrationServiceGrant, Key: "external-deployment.admit", Definition: json.RawMessage(`{}`)}}))
	require.NoError(t, PutPluginEnablement(t.Context(), tx, installation, PluginEnablement{OrgID: orgs[2], State: PluginEnablementEnabled, Timezone: "UTC", ApprovalPolicies: map[string]string{}}, fixtureOperator))
	require.NoError(t, tx.Commit())

	require.Equal(t, []string{"dev.example.second", "dev.example.watcher"}, subscribers(orgs[1]), "subscribers are sorted")
	require.Equal(t, []string{fixturePlugin}, subscribers(orgs[0]))
	require.Empty(t, subscribers(orgs[2]), "other service grants are not event subscriptions")
	require.Empty(t, subscribers("unconfigured"))
	journaling, _ := journalState(t, db, orgs[1])
	require.True(t, journaling)
	var tracked bool
	require.NoError(t, db.(*databaser).QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM extension_resource_event_journal_state WHERE org_id=$1 AND journaling)`, orgs[2]).Scan(&tracked))
	require.False(t, tracked, "an organization without a subscriber is not journaled")

	_, err = db.(*databaser).ExecContext(t.Context(), `UPDATE plugin_enablements SET state='disabled' WHERE org_id=$1 AND plugin_id='dev.example.watcher'`, orgs[1])
	require.NoError(t, err)
	require.Equal(t, []string{"dev.example.second", "dev.example.watcher"}, subscribers(orgs[1]), "a safety-only plugin still reconciles from the journal")
	unsubscribeFromResourceEvents(t, db, orgs[1], "dev.example.watcher")
	require.Equal(t, []string{"dev.example.second"}, subscribers(orgs[1]))
}

func requireConflictCode(t *testing.T, err error, code string) {
	t.Helper()
	conflict, ok := IsErrConflict(err)
	require.True(t, ok, "expected conflict %s, got %v", code, err)
	require.Equal(t, code, conflict.Code)
}
