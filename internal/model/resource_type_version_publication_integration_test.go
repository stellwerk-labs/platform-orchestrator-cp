//go:build integration

package model

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func TestResourceTypeVersionPublicationSerializesWriters(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	ctx := t.Context()
	actor := uuid.New()
	org := "contracts-" + uuid.NewString()
	_, err := db.CreateOrg(ctx, nil, &Org{Id: org, CreatedBy: actor, CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	_, err = db.CreateResourceType(ctx, nil, &ResourceType{OrgId: opt.Of(org), Id: "service", OutputsSchema: map[string]any{}, CreatedAt: time.Now()})
	require.NoError(t, err)
	var writers sync.WaitGroup
	results := make(chan error, 5)
	for range 5 {
		writers.Go(func() {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			_, err = db.PublishResourceTypeVersion(ctx, tx, org, "service", ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: map[string]any{}, ModuleContract: nil, Actor: actor, Reason: "Publish reviewed contract"})
			if err == nil {
				err = tx.Commit()
			}
			results <- err
		})
	}
	writers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		_, ok := IsErrConflict(err)
		require.True(t, ok, "unexpected publication failure: %v", err)
	}
	require.Equal(t, 1, successes)
	versions, err := db.ListResourceTypeVersions(ctx, nil, org, "service", 0, 2)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, int64(1), versions[0].Version)
	require.Equal(t, int64(2), versions[1].Version)
	page, err := db.ListResourceTypeVersions(ctx, nil, org, "service", 1, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, versions[1].UUID, page[0].UUID)
	_, err = db.GetResourceTypeVersion(ctx, nil, "another-organization", "service", 2)
	_, missing := IsErrNotFound(err)
	require.True(t, missing)
}

func TestResourceTypePublicationSerializesWithCatalogueDeletion(t *testing.T) {
	db := externalOperationDatabaseWith(t)
	ctx, actor, org := t.Context(), uuid.New(), "contracts-"+uuid.NewString()
	_, err := db.CreateOrg(ctx, nil, &Org{Id: org, CreatedBy: actor, CreatedAt: time.Now(), Source: OrgSourceInternal, Plan: OrgPlanCustom})
	require.NoError(t, err)
	_, err = db.CreateResourceType(ctx, nil, &ResourceType{OrgId: opt.Of(org), Id: "service", OutputsSchema: map[string]any{}, CreatedAt: time.Now()})
	require.NoError(t, err)
	hold, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = hold.Rollback() }()
	require.NoError(t, lockResourceTypeIdentity(ctx, hold, "service"))
	waiting := func(name string) {
		t.Helper()
		require.Eventually(t, func() bool {
			var found bool
			err := db.(*databaser).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%')`, name).Scan(&found)
			return err == nil && found
		}, 3*time.Second, 10*time.Millisecond)
	}
	run := func(name string, action func(Tx) error) <-chan error {
		result := make(chan error, 1)
		go func() {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				result <- err
				return
			}
			defer func() { _ = tx.Rollback() }()
			if _, err = tx.ExecContext(ctx, `SELECT set_config('application_name',$1,true)`, name); err == nil {
				err = action(tx)
			}
			if err == nil {
				err = tx.Commit()
			}
			result <- err
		}()
		return result
	}
	publisher := "contract-publish-" + uuid.NewString()
	published := run(publisher, func(tx Tx) error {
		_, err := db.PublishResourceTypeVersion(ctx, tx, org, "service", ResourceTypeVersionPublication{Version: 2, ExpectedResourceVersion: 1, OutputSchema: map[string]any{}, ModuleContract: nil, Actor: actor, Reason: "Publish reviewed interface"})
		return err
	})
	waiting(publisher)
	deleter := "contract-delete-" + uuid.NewString()
	deleted := run(deleter, func(tx Tx) error { return db.DeleteResourceType(ctx, tx, &org, "service") })
	waiting(deleter)
	require.NoError(t, hold.Commit())
	require.NoError(t, <-published)
	_, conflict := IsErrConflict(<-deleted)
	require.True(t, conflict, "concurrent deletion must return a domain conflict, not a constraint failure")
	version, err := db.GetResourceTypeVersion(ctx, nil, org, "service", 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), version.Version)
}
