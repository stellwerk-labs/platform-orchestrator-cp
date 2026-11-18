//go:build integration

package model

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestObservationDeletedTargetsPruneAtomicallyAndReplayExactly(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	require.NoError(t, exerciseSQLMigrationDown(t, db, 57, true))
	original, err := db.GetEnvironmentByUuid(t.Context(), nil, fixture.OrgID, fixture.Intent.Environments[0].EnvironmentID, GetModeDefault)
	require.NoError(t, err)
	copy := *original
	copy.Id = "survivor"
	survivor, err := db.CreateEnvironment(t.Context(), nil, &copy)
	require.NoError(t, err)
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Monitor explicit Environments", Targets: append(fixture.Intent.Environments, ExternalOperationEnvironment{ProjectID: survivor.ProjectUuid, EnvironmentID: survivor.Uuid})}
	apply := func() (*ObservationGrant, error) {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		value, err := ApplyObservationGrantCommandTx(t.Context(), tx, command, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		return value, tx.Commit()
	}
	_, err = apply()
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "DELETE FROM envs WHERE org_id=$1 AND uuid=$2", fixture.OrgID, original.Uuid)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE envs SET status='deleting' WHERE org_id=$1 AND uuid=$2", fixture.OrgID, survivor.Uuid)
	require.NoError(t, err)
	deleted, err := DeletedObservationTargetsTx(t.Context(), tx, fixture.OrgID, []uuid.UUID{original.Uuid, survivor.Uuid})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{original.Uuid}, deleted, "inactivity is not deletion")
	require.NoError(t, tx.Commit())
	command.ExpectedRevision = 1
	command.RequestID = uuid.NewString()
	grant, err := apply()
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{original.Uuid}, grant.PrunedEnvironmentIDs)
	require.Equal(t, []ExternalOperationEnvironment{{ProjectID: survivor.ProjectUuid, EnvironmentID: survivor.Uuid}}, grant.Targets)
	replay, err := apply()
	require.NoError(t, err)
	require.Equal(t, grant, replay)
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	readback, err := ReadObservationGrantTx(t.Context(), tx, command.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, err)
	require.Equal(t, grant, readback)
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, exerciseSQLMigrationDown(t, db, 57, false), "coordinated restore")
	command.ExpectedRevision = 2
	command.RequestID = uuid.NewString()
	command.Targets = []ExternalOperationEnvironment{{ProjectID: survivor.ProjectUuid, EnvironmentID: uuid.New()}}
	_, err = apply()
	require.ErrorIs(t, err, ErrObservationTargetsUnavailable, "a new unknown target must not be silently dropped")
	command.RequestID = uuid.NewString()
	command.Targets = grant.Targets
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), "DELETE FROM envs WHERE org_id=$1 AND uuid=$2", fixture.OrgID, survivor.Uuid)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	grant, err = apply()
	require.NoError(t, err)
	require.Equal(t, "revoked", grant.State)
	require.Empty(t, grant.Targets)
	require.Equal(t, []uuid.UUID{survivor.Uuid}, grant.PrunedEnvironmentIDs)
}

func TestObservationDeletedClosureFormatBlocksDowngrade(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	body := []byte(`{"request_id":"format-fixture"}`)
	closure := ObservationClosure{Kind: ObservationAdmission, RequestID: uuid.New(), RequestDigest: plugincontext.Digest(body), Body: body, ServiceActorID: uuid.New()}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = RecordObservationClosureTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", closure)
	require.NoError(t, err)
	response, err := json.Marshal(map[string]any{"request_id": closure.RequestID, "disposition": "not_started", "reason": "deleted_target"})
	require.NoError(t, err)
	require.NoError(t, CompleteObservationClosureTx(t.Context(), tx, fixture.OrgID, "dev.example.observer", closure.Kind, closure.RequestID, "not_started", response))
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, exerciseSQLMigrationDown(t, db, 57, false), "coordinated restore")
}
