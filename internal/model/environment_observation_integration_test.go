//go:build integration

package model

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	"github.com/stretchr/testify/require"
)

func TestObservationGrantsAreExactSeparateAndRevisionFenced(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	now := time.Now().UTC()
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Monitor explicit Environment", Targets: fixture.Intent.Environments}
	apply := func(cmd ObservationGrantCommand) (*ObservationGrant, error) {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		grant, err := ApplyObservationGrantCommandTx(t.Context(), tx, cmd, now)
		if err != nil {
			return nil, err
		}
		return grant, tx.Commit()
	}
	grant, err := apply(command)
	require.NoError(t, err)
	require.Equal(t, int64(1), grant.Revision)
	require.Equal(t, command.RequestID, grant.LastEffectRequestID)
	replay, err := apply(command)
	require.NoError(t, err)
	require.Equal(t, grant, replay)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var oldCount int
	err = tx.QueryRowContext(t.Context(), `SELECT count(*) FROM external_operation_grants WHERE org_id=$1`, command.OrgID).Scan(&oldCount)
	require.NoError(t, err)
	require.Zero(t, oldCount, "observation grants must not fabricate Module operation authority")
	require.NoError(t, tx.Rollback())
	target := command.Targets[0]
	require.NoError(t, CheckObservationGrant(grant, 1, target.ProjectID, target.EnvironmentID, now))
	require.Error(t, CheckObservationGrant(grant, 1, uuid.New(), target.EnvironmentID, now))
	require.Error(t, CheckObservationGrant(grant, 1, target.ProjectID, uuid.New(), now))
	expires := now.Add(time.Minute)
	expired := *grant
	expired.ExpiresAt = &expires
	require.Error(t, CheckObservationGrant(&expired, 1, target.ProjectID, target.EnvironmentID, expires))
	changed := command
	changed.Reason = "Changed body"
	_, err = apply(changed)
	require.ErrorContains(t, err, "another command")
	wrong := command
	wrong.GrantID = uuid.New()
	wrong.RequestID = uuid.NewString()
	wrong.Targets = []ExternalOperationEnvironment{{ProjectID: uuid.New(), EnvironmentID: target.EnvironmentID}}
	_, err = apply(wrong)
	require.ErrorContains(t, err, "project changed")
	commands := make([]ObservationGrantCommand, 2)
	for i := range commands {
		commands[i] = command
		commands[i].ExpectedRevision = 1
		commands[i].RequestID = uuid.NewString()
	}
	commands[1].Action = "revoke"
	commands[1].Targets = nil
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, cmd := range commands {
		wg.Go(func() { _, err := apply(cmd); results <- err })
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorContains(t, err, "revision changed")
		}
	}
	require.Equal(t, 1, succeeded)
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	current, err := ReadObservationGrantTx(t.Context(), tx, command.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, err)
	require.Equal(t, int64(2), current.Revision)
	require.Error(t, CheckObservationGrant(current, 1, target.ProjectID, target.EnvironmentID, now))
	require.NoError(t, tx.Rollback())
}

func TestObservationExactRequestsSurviveAmbiguityAndRejectBodyReuse(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	now := time.Now().UTC()
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "replace", Reason: "Monitor target", Targets: fixture.Intent.Environments}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = ApplyObservationGrantCommandTx(t.Context(), tx, command, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	target := command.Targets[0]
	body := json.RawMessage(`{"contract":"environment-observation/v1"}`)
	record := ObservationRequestRecord{ActorID: fixture.ActorID, GrantID: command.GrantID, GrantRevision: 1, ProjectUUID: target.ProjectID, EnvironmentUUID: target.EnvironmentID, RequestID: uuid.New(), RequestDigest: plugincontext.Digest(body), Body: body, Deadline: now.Add(time.Minute)}
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, RecordObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, record))
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	loaded, err := ReadObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, record.RequestID, uuid.Nil)
	require.NoError(t, err)
	require.Nil(t, loaded.Response)
	require.Equal(t, record.GrantID, loaded.GrantID)
	require.NoError(t, RecordObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, record))
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	changed := record
	changed.RequestDigest = plugincontext.Digest([]byte("other"))
	require.ErrorContains(t, RecordObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, changed), "different exact request")
	require.NoError(t, tx.Rollback())
	run := uuid.New()
	receipt := json.RawMessage(`{"disposition":"accepted"}`)
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, CompleteObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, record.RequestID, 200, receipt, &run))
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	loaded, err = ReadObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, uuid.Nil, run)
	require.NoError(t, err)
	require.JSONEq(t, string(receipt), string(loaded.Response))
	require.Equal(t, run, *loaded.RunID)
	require.NoError(t, tx.Rollback())
	// Schedule replacement/revocation cannot strand a previously accepted run.
	revoke := command
	revoke.ExpectedRevision = 1
	revoke.Action = "revoke"
	revoke.Targets = nil
	revoke.RequestID = uuid.NewString()
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	grant, err := ApplyObservationGrantCommandTx(t.Context(), tx, revoke, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Error(t, CheckObservationGrant(grant, record.GrantRevision, target.ProjectID, target.EnvironmentID, now), "old grant cannot create new work")
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	retained, err := ReadObservationRequestTx(t.Context(), tx, command.OrgID, command.PluginID, record.RequestID, uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, fixture.ActorID, retained.ActorID)
	require.JSONEq(t, string(receipt), string(retained.Response))
	_, err = ReadObservationRequestTx(t.Context(), tx, "other-org", command.PluginID, record.RequestID, uuid.Nil)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = ReadObservationRequestTx(t.Context(), tx, command.OrgID, "dev.example.other", uuid.Nil, run)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, tx.Rollback())
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	attempt := ObservationRedeployment{RequestID: uuid.New(), ObservationRequestID: record.RequestID, RunID: run, ActorID: fixture.ActorID, RequestDigest: plugincontext.Digest([]byte("redeploy")), Body: json.RawMessage(`{"baseline_id":"fixture"}`)}
	require.NoError(t, RecordObservationRedeploymentTx(t.Context(), tx, command.OrgID, command.PluginID, attempt))
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	persisted, err := ReadObservationRedeploymentTx(t.Context(), tx, command.OrgID, command.PluginID, attempt.RequestID)
	require.NoError(t, err)
	require.Equal(t, attempt.ActorID, persisted.ActorID)
	require.NoError(t, RecordObservationRedeploymentTx(t.Context(), tx, command.OrgID, command.PluginID, attempt))
	changedAttempt := attempt
	changedAttempt.ActorID = uuid.New()
	require.ErrorContains(t, RecordObservationRedeploymentTx(t.Context(), tx, command.OrgID, command.PluginID, changedAttempt), "another exact request or actor")
	_, err = ReadObservationRedeploymentTx(t.Context(), tx, command.OrgID, "dev.example.other", attempt.RequestID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, tx.Rollback())
}

func TestObservationGrantReceiptReplaysWithFreshSignedSourceProof(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	now := time.Now().UTC().Truncate(time.Second)
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	body := []byte(`{"enabled":true,"environment_ids":["reviewed-target"]}`)
	requestID := uuid.NewString()
	path := "/orgs/" + fixture.OrgID + "/configuration"
	claimsAt := func(at time.Time) (string, plugincontext.Claims) {
		t.Helper()
		signed, err := plugincontext.Sign(key, plugincontext.Claims{Version: 1, Issuer: "cp", Audience: "plugin:dev.example.observer", PluginID: "dev.example.observer", OrgID: fixture.OrgID, ActorID: fixture.ActorID.String(), OperationID: "configure", Method: "PUT", Path: path, BodySHA256: plugincontext.Digest(body), RequestID: requestID, IssuedAt: at.Unix(), ExpiresAt: at.Add(plugincontext.MaxLifetime).Unix()}, at)
		require.NoError(t, err)
		claims, err := plugincontext.Verify(key.Public().(ed25519.PublicKey), signed, plugincontext.Request{Issuer: "cp", Audience: "plugin:dev.example.observer", PluginID: "dev.example.observer", OrgID: fixture.OrgID, OperationID: "configure", Method: "PUT", Path: path, Body: body, IdempotencyKey: requestID}, at)
		require.NoError(t, err)
		return signed, claims
	}
	firstProof, firstClaims := claimsAt(now)
	secondProof, secondClaims := claimsAt(now.Add(5 * time.Second))
	require.NotEqual(t, firstProof, secondProof)
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: firstClaims.OperationID, SourceDigest: firstClaims.BodySHA256, RequestID: firstClaims.RequestID, Action: "replace", Reason: "Monitor reviewed exact target", Targets: fixture.Intent.Environments}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	first, err := ApplyObservationGrantCommandTx(t.Context(), tx, command, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	command.SourceOperation = secondClaims.OperationID
	command.SourceDigest = secondClaims.BodySHA256
	command.RequestID = secondClaims.RequestID
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	replayed, err := ApplyObservationGrantCommandTx(t.Context(), tx, command, now.Add(5*time.Second))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, first, replayed)
}

func TestObservationInitialRevokeCreatesReplayableTombstone(t *testing.T) {
	db := externalOperationDatabase(t)
	fixture := externalGrantFixture(t, db)
	now := time.Now().UTC()
	command := ObservationGrantCommand{OrgID: fixture.OrgID, PluginID: "dev.example.observer", GrantID: uuid.New(), ActorID: fixture.ActorID, SourceOperation: "configure", SourceDigest: fixture.SourceDigest, RequestID: uuid.NewString(), Action: "revoke", Reason: "No monitored targets"}
	apply := func(cmd ObservationGrantCommand) *ObservationGrant {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		grant, err := ApplyObservationGrantCommandTx(t.Context(), tx, cmd, now)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return grant
	}
	tombstone := apply(command)
	require.Equal(t, int64(1), tombstone.Revision)
	require.Equal(t, "revoked", tombstone.State)
	require.Empty(t, tombstone.Targets)
	require.Nil(t, tombstone.ExpiresAt)
	target := fixture.Intent.Environments[0]
	require.Error(t, CheckObservationGrant(tombstone, 1, target.ProjectID, target.EnvironmentID, now))
	require.Equal(t, tombstone, apply(command))
	replacement := command
	replacement.RequestID, replacement.ExpectedRevision, replacement.Action = uuid.NewString(), 1, "replace"
	replacement.Targets = fixture.Intent.Environments
	active := apply(replacement)
	require.Equal(t, int64(2), active.Revision)
	require.NoError(t, CheckObservationGrant(active, 2, target.ProjectID, target.EnvironmentID, now))
	require.Equal(t, tombstone, apply(command), "old command receipt does not revoke a later configuration")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	current, err := ReadObservationGrantTx(t.Context(), tx, command.OrgID, command.PluginID, command.GrantID)
	require.NoError(t, err)
	require.Equal(t, int64(2), current.Revision)
	require.NoError(t, tx.Rollback())
	removal := command
	removal.RequestID, removal.ExpectedRevision = uuid.NewString(), 2
	require.Equal(t, int64(3), apply(removal).Revision)
}
