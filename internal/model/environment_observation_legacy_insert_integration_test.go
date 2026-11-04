//go:build integration

package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/require"
)

type observationLegacyJournal struct {
	db                                              Databaser
	org, plugin                                     string
	actor, grant, project, environment, parent, run uuid.UUID
}

func newObservationLegacyJournal(t *testing.T) *observationLegacyJournal {
	t.Helper()
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	target := fixture.Intent.Environments[0]
	f := &observationLegacyJournal{db: db, org: fixture.OrgID, plugin: "dev.example.observer", actor: fixture.ActorID, grant: uuid.New(), project: target.ProjectID, environment: target.EnvironmentID, parent: uuid.New(), run: uuid.New()}
	tx, e := db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	_, e = ApplyObservationGrantCommandTx(t.Context(), tx, ObservationGrantCommand{OrgID: f.org, PluginID: f.plugin, GrantID: f.grant, ActorID: f.actor, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Legacy journal boundary fixture", Targets: fixture.Intent.Environments}, time.Now().UTC())
	require.NoError(t, e)
	require.NoError(t, f.insert(t.Context(), tx, ObservationAdmission, f.parent, false))
	require.NoError(t, tx.Commit())
	return f
}

// insert deliberately issues the historical raw SQL shape, without invoking the
// new model open-check. A mixed-version process cannot bypass the DB trigger.
func (f *observationLegacyJournal) insert(ctx context.Context, tx Tx, kind string, id uuid.UUID, onConflict bool) error {
	suffix := ""
	if onConflict {
		suffix = " ON CONFLICT DO NOTHING"
	}
	if kind == ObservationAdmission {
		_, e := tx.ExecContext(ctx, `INSERT INTO environment_observation_requests(org_id,plugin_id,request_id,grant_id,grant_revision,project_uuid,environment_uuid,request_digest,actor_id,request_body,admission_deadline) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,'{}',now())`+suffix, f.org, f.plugin, id, f.grant, f.project, f.environment, plugincontext.Digest([]byte(`{}`)), f.actor)
		return e
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO environment_observation_redeployments(org_id,plugin_id,request_id,observation_request_id,run_id,actor_id,request_digest,request_body) VALUES($1,$2,$3,$4,$5,$6,$7,'{}')`+suffix, f.org, f.plugin, id, f.parent, f.run, f.actor, plugincontext.Digest([]byte(`{}`)))
	return e
}
func (f *observationLegacyJournal) close(t *testing.T, tx Tx, kind string, id uuid.UUID, state string) {
	t.Helper()
	body := json.RawMessage(`{}`)
	c := ObservationClosure{Kind: kind, RequestID: id, RequestDigest: plugincontext.Digest(body), Body: body, ServiceActorID: uuid.New()}
	if kind == ObservationRedeploymentAdmission {
		c.RunID = &f.run
	}
	_, e := RecordObservationClosureTx(t.Context(), tx, f.org, f.plugin, c)
	require.NoError(t, e)
	if state != "close_requested" {
		require.NoError(t, CompleteObservationClosureTx(t.Context(), tx, f.org, f.plugin, kind, id, state, json.RawMessage(`{"disposition":"fixture"}`)))
	}
}
func requireObservationClosedInsert(t *testing.T, err error) {
	t.Helper()
	var pg *pq.Error
	require.ErrorAs(t, err, &pg)
	require.EqualValues(t, "23514", pg.Code)
	require.Equal(t, "environment_observation_admission_closed", pg.Constraint)
}
func waitObservationAdvisoryBlock(t *testing.T, db Databaser, blockingPID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tx, e := db.BeginTx(t.Context(), nil)
		require.NoError(t, e)
		var blocked bool
		e = tx.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND wait_event='advisory')`, blockingPID).Scan(&blocked)
		require.NoError(t, e)
		require.NoError(t, tx.Rollback())
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("competing statement did not block on the exact advisory request fence")
}
func observationBackendPID(t *testing.T, tx Tx) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

func TestObservationLegacyInsertAfterLocalSealCannotReachForward(t *testing.T) {
	f := newObservationLegacyJournal(t)
	for _, kind := range []string{ObservationAdmission, ObservationRedeploymentAdmission} {
		for _, state := range []string{"close_requested", "accepted", "not_started"} {
			for _, replay := range []bool{false, true} {
				id := uuid.New()
				if replay {
					tx, e := f.db.BeginTx(t.Context(), nil)
					require.NoError(t, e)
					require.NoError(t, f.insert(t.Context(), tx, kind, id, false))
					require.NoError(t, tx.Commit())
				}
				closed, e := f.db.BeginTx(t.Context(), nil)
				require.NoError(t, e)
				f.close(t, closed, kind, id, state)
				require.NoError(t, closed.Commit())
				var remoteCalls atomic.Int32
				for _, onConflict := range []bool{false, true} {
					legacy, e := f.db.BeginTx(t.Context(), nil)
					require.NoError(t, e)
					e = f.insert(t.Context(), legacy, kind, id, onConflict)
					if e == nil {
						e = legacy.Commit()
						if e == nil {
							remoteCalls.Add(1)
						}
					}
					requireObservationClosedInsert(t, e)
					require.Error(t, legacy.Commit(), "the old process cannot commit routing before sending")
					_ = legacy.Rollback()
				}
				require.Zero(t, remoteCalls.Load())
			}
		}
	}
}

func TestObservationLegacyInsertWaitsForUncommittedClosureDecision(t *testing.T) {
	f := newObservationLegacyJournal(t)
	for _, kind := range []string{ObservationAdmission, ObservationRedeploymentAdmission} {
		for _, commit := range []bool{false, true} {
			id := uuid.New()
			closure, e := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, e)
			f.close(t, closure, kind, id, "not_started")
			pid := observationBackendPID(t, closure)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			inserted := make(chan error, 1)
			go func() {
				tx, e := f.db.BeginTx(ctx, nil)
				if e != nil {
					inserted <- e
					return
				}
				defer func() { _ = tx.Rollback() }()
				e = f.insert(ctx, tx, kind, id, true)
				if e == nil {
					e = tx.Commit()
				}
				inserted <- e
			}()
			waitObservationAdvisoryBlock(t, f.db, pid)
			if commit {
				require.NoError(t, closure.Commit())
			} else {
				require.NoError(t, closure.Rollback())
			}
			e = <-inserted
			cancel()
			if commit {
				requireObservationClosedInsert(t, e)
			} else {
				require.NoError(t, e)
			}
		}
	}
}
func TestObservationUncommittedLegacyInsertPreventsFalseMissingJournal(t *testing.T) {
	f := newObservationLegacyJournal(t)
	for _, kind := range []string{ObservationAdmission, ObservationRedeploymentAdmission} {
		for _, commit := range []bool{false, true} {
			id := uuid.New()
			legacy, e := f.db.BeginTx(t.Context(), nil)
			require.NoError(t, e)
			require.NoError(t, f.insert(t.Context(), legacy, kind, id, true))
			pid := observationBackendPID(t, legacy)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			observed := make(chan bool, 1)
			failed := make(chan error, 1)
			go func() {
				tx, e := f.db.BeginTx(ctx, nil)
				if e != nil {
					failed <- e
					return
				}
				defer func() { _ = tx.Rollback() }()
				if e = LockObservationAdmissionTx(ctx, tx, f.org, f.plugin, kind, id); e != nil {
					failed <- e
					return
				}
				var original *uuid.UUID
				if kind == ObservationAdmission {
					record, err := ReadObservationRequestTx(ctx, tx, f.org, f.plugin, id, uuid.Nil)
					e = err
					if err == nil {
						original = &record.ActorID
					}
				} else {
					record, err := ReadObservationRedeploymentTx(ctx, tx, f.org, f.plugin, id)
					e = err
					if err == nil {
						original = &record.ActorID
					}
				}
				if e != nil && !errors.Is(e, sql.ErrNoRows) {
					failed <- e
					return
				}
				body := json.RawMessage(`{}`)
				closure := ObservationClosure{Kind: kind, RequestID: id, RequestDigest: plugincontext.Digest(body), Body: body, OriginalActorID: original, ServiceActorID: uuid.New()}
				if kind == ObservationRedeploymentAdmission {
					closure.RunID = &f.run
				}
				_, e = RecordObservationClosureTx(ctx, tx, f.org, f.plugin, closure)
				if e == nil {
					e = tx.Commit()
				}
				if e != nil {
					failed <- e
					return
				}
				observed <- original != nil
			}()
			waitObservationAdvisoryBlock(t, f.db, pid)
			if commit {
				require.NoError(t, legacy.Commit())
			} else {
				require.NoError(t, legacy.Rollback())
			}
			select {
			case present := <-observed:
				require.Equal(t, commit, present, "a committed old intent routes to DP; only a rolled-back insert permits local absence")
			case e := <-failed:
				require.NoError(t, e)
			case <-ctx.Done():
				t.Fatal("resolution did not finish")
			}
			cancel()
		}
	}
}

func TestObservationSQLAdmissionLockMatchesLegacyGoEncoding(t *testing.T) {
	f := newObservationLegacyJournal(t)
	for _, kind := range []string{ObservationAdmission, ObservationRedeploymentAdmission} {
		// The serializer must also agree beyond normal slug input, including Go's
		// HTML and U+2028/U+2029 escapes and JSON string escaping.
		org := "lock<&>\u2028\u2029\"\\,scope"
		id := uuid.New()
		held, e := f.db.BeginTx(t.Context(), nil)
		require.NoError(t, e)
		require.NoError(t, lockExternalGrant(t.Context(), held, org, "observation-admission:"+kind+":"+f.plugin, id))
		pid := observationBackendPID(t, held)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		done := make(chan error, 1)
		go func() {
			tx, e := f.db.BeginTx(ctx, nil)
			if e != nil {
				done <- e
				return
			}
			defer func() { _ = tx.Rollback() }()
			e = LockObservationAdmissionTx(ctx, tx, org, f.plugin, kind, id)
			if e == nil {
				e = tx.Commit()
			}
			done <- e
		}()
		waitObservationAdvisoryBlock(t, f.db, pid)
		require.NoError(t, held.Rollback())
		require.NoError(t, <-done)
		cancel()
	}
}

func TestObservationLegacyInsertRejectsFrozenTransactionSnapshots(t *testing.T) {
	f := newObservationLegacyJournal(t)
	for _, kind := range []string{ObservationAdmission, ObservationRedeploymentAdmission} {
		tx, e := f.db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		require.NoError(t, e)
		e = f.insert(t.Context(), tx, kind, uuid.New(), true)
		var pg *pq.Error
		require.ErrorAs(t, e, &pg)
		require.Equal(t, "environment_observation_admission_isolation", pg.Constraint)
		require.Error(t, tx.Commit())
		_ = tx.Rollback()
	}
}
