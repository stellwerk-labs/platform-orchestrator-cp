//go:build integration

package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/require"
)

func TestObservationRecoveryMigrationBackfillsHistoricalMembershipAndRetainsClosures(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	now := time.Now().UTC()
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Reviewed explicit scope", Targets: fixture.Intent.Environments}
	apply := func() {
		tx, e := db.BeginTx(t.Context(), nil)
		require.NoError(t, e)
		_, e = ApplyObservationGrantCommandTx(t.Context(), tx, command, now)
		require.NoError(t, e)
		require.NoError(t, tx.Commit())
	}
	apply()
	command.ExpectedRevision = 1
	command.RequestID = uuid.NewString()
	command.Action = "revoke"
	command.Targets = nil
	apply()
	require.NoError(t, goose.DownToContext(t.Context(), db.(*databaser).DB, "migrations", 52))
	require.NoError(t, goose.UpContext(t.Context(), db.(*databaser).DB, "migrations"))
	tx, e := db.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	target := fixture.Intent.Environments[0]
	require.NoError(t, CheckIssuedObservationTargetTx(t.Context(), tx, fixture.OrgID, command.PluginID, command.GrantID, 1, target.ProjectID, target.EnvironmentID))
	require.Error(t, CheckIssuedObservationTargetTx(t.Context(), tx, fixture.OrgID, command.PluginID, command.GrantID, 2, target.ProjectID, target.EnvironmentID))
	require.Error(t, CheckIssuedObservationTargetTx(t.Context(), tx, "foreign", command.PluginID, command.GrantID, 1, target.ProjectID, target.EnvironmentID))
	require.Error(t, CheckIssuedObservationTargetTx(t.Context(), tx, fixture.OrgID, "dev.example.foreign", command.GrantID, 1, target.ProjectID, target.EnvironmentID))
	require.Error(t, CheckIssuedObservationTargetTx(t.Context(), tx, fixture.OrgID, command.PluginID, command.GrantID, 1, uuid.New(), target.EnvironmentID))
	raw := json.RawMessage(`{"request_id":"fixture"}`)
	closure := ObservationClosure{Kind: ObservationAdmission, RequestID: uuid.New(), RequestDigest: plugincontext.Digest(raw), Body: raw, ServiceActorID: uuid.New()}
	_, e = RecordObservationClosureTx(t.Context(), tx, fixture.OrgID, command.PluginID, closure)
	require.NoError(t, e)
	require.NoError(t, CompleteObservationClosureTx(t.Context(), tx, fixture.OrgID, command.PluginID, closure.Kind, closure.RequestID, "not_started", json.RawMessage(`{"disposition":"not_started"}`)))
	require.NoError(t, tx.Commit())
	require.ErrorContains(t, goose.DownToContext(t.Context(), db.(*databaser).DB, "migrations", 52), "cannot discard immutable observation no-forward tombstones")
}

func TestObservationTenThousandTargetsUseIndexedMetadataAndHistoricalScope(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	ctx := t.Context()
	environment, e := db.GetEnvironmentByUuid(ctx, nil, fixture.OrgID, fixture.Intent.Environments[0].EnvironmentID, GetModeDefault)
	require.NoError(t, e)
	tx, e := db.BeginTx(ctx, nil)
	require.NoError(t, e)
	_, e = tx.ExecContext(ctx, `INSERT INTO envs(org_id,org_uuid,project_id,project_uuid,id,env_type_id,env_type_uuid,display_name,created_at,updated_at,status) SELECT $1,$2,$3,$4,'scope-'||n::text,$5,$6,'Scale scope',now(),now(),'active' FROM generate_series(1,9999) n`, environment.OrgId, environment.OrgUuid, environment.ProjectId, environment.ProjectUuid, environment.EnvTypeId, environment.EnvTypeUuid)
	require.NoError(t, e)
	rows, e := tx.QueryContext(ctx, `SELECT project_uuid,uuid FROM envs WHERE org_id=$1 ORDER BY uuid`, fixture.OrgID)
	require.NoError(t, e)
	defer func() { _ = rows.Close() }()
	targets := []ExternalOperationEnvironment{}
	for rows.Next() {
		var target ExternalOperationEnvironment
		require.NoError(t, rows.Scan(&target.ProjectID, &target.EnvironmentID))
		targets = append(targets, target)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, targets, 10000)
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Reviewed ten thousand explicit targets", Targets: targets}
	_, e = ApplyObservationGrantCommandTx(ctx, tx, command, time.Now().UTC())
	require.NoError(t, e)
	require.NoError(t, tx.Commit())
	tx, e = db.BeginTx(ctx, nil)
	require.NoError(t, e)
	metadata, e := ReadObservationGrantMetadataTx(ctx, tx, fixture.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, e)
	require.Nil(t, metadata.Targets, "admission must not decode the selection snapshot")
	for _, target := range targets {
		require.NoError(t, CheckObservationGrantTargetTx(ctx, tx, metadata, 1, target.ProjectID, target.EnvironmentID, time.Now().UTC()))
	}
	require.Error(t, CheckObservationGrantTargetTx(ctx, tx, metadata, 1, targets[0].ProjectID, uuid.New(), time.Now().UTC()))
	_, e = tx.ExecContext(ctx, `ANALYZE environment_observation_grant_targets`)
	require.NoError(t, e)
	var plan string
	require.NoError(t, tx.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) SELECT EXISTS(SELECT 1 FROM environment_observation_grant_targets WHERE org_id=$1 AND plugin_id=$2 AND grant_id=$3 AND revision=1 AND project_uuid=$4 AND environment_uuid=$5)`, fixture.OrgID, command.PluginID, command.GrantID, targets[9999].ProjectID, targets[9999].EnvironmentID).Scan(&plan))
	require.Contains(t, plan, "Index")
	require.NotContains(t, plan, `"Node Type": "Seq Scan"`)
	require.NoError(t, tx.Commit())
	command.ExpectedRevision = 1
	command.RequestID = uuid.NewString()
	command.Action = "revoke"
	command.Targets = nil
	tx, e = db.BeginTx(ctx, nil)
	require.NoError(t, e)
	_, e = ApplyObservationGrantCommandTx(ctx, tx, command, time.Now().UTC())
	require.NoError(t, e)
	require.NoError(t, tx.Commit())
	tx, e = db.BeginTx(ctx, nil)
	require.NoError(t, e)
	defer func() { _ = tx.Rollback() }()
	metadata, e = ReadObservationGrantMetadataTx(ctx, tx, fixture.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, e)
	require.Nil(t, metadata.Targets)
	require.Error(t, CheckObservationGrantTargetTx(ctx, tx, metadata, 1, targets[9999].ProjectID, targets[9999].EnvironmentID, time.Now().UTC()))
	require.NoError(t, CheckIssuedObservationTargetTx(ctx, tx, fixture.OrgID, command.PluginID, command.GrantID, 1, targets[9999].ProjectID, targets[9999].EnvironmentID))
	require.Error(t, CheckIssuedObservationTargetTx(ctx, tx, fixture.OrgID, command.PluginID, command.GrantID, 2, targets[9999].ProjectID, targets[9999].EnvironmentID))
	full, e := ReadObservationGrantTx(ctx, tx, fixture.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, e)
	require.Empty(t, full.Targets)
	require.Contains(t, plan, "environment_observation_grant_targets")
}

func TestObservationClosureSharesTheInFlightForwardLock(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	plugin := "dev.example.observer"
	id := uuid.New()
	ctx := t.Context()
	forward, e := db.BeginTx(ctx, nil)
	require.NoError(t, e)
	require.NoError(t, CheckObservationAdmissionOpenTx(ctx, forward, fixture.OrgID, plugin, ObservationAdmission, id))
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	closer, e := db.BeginTx(blocked, nil)
	require.NoError(t, e)
	raw := json.RawMessage(`{"request_id":"locked"}`)
	command := ObservationClosure{Kind: ObservationAdmission, RequestID: id, RequestDigest: plugincontext.Digest(raw), Body: raw, ServiceActorID: uuid.New()}
	_, e = RecordObservationClosureTx(blocked, closer, fixture.OrgID, plugin, command)
	require.Error(t, e, "resolution cannot commit while forwarding holds the exact key fence")
	_ = closer.Rollback()
	require.NoError(t, forward.Rollback())
	closer, e = db.BeginTx(ctx, nil)
	require.NoError(t, e)
	_, e = RecordObservationClosureTx(ctx, closer, fixture.OrgID, plugin, command)
	require.NoError(t, e)
	require.NoError(t, closer.Commit())
	delayed, e := db.BeginTx(ctx, nil)
	require.NoError(t, e)
	require.Error(t, CheckObservationAdmissionOpenTx(ctx, delayed, fixture.OrgID, plugin, ObservationAdmission, id))
	require.NoError(t, delayed.Rollback())
}
