package model

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

const ObservationAdmission = "observation"
const ObservationRedeploymentAdmission = "redeployment"

// ObservationClosure is a monotonic no-forward decision, not user authority.
// OriginalActorID is absent when Core never received an authorized submission;
// ServiceActorID records the actual installation service requesting closure.
type ObservationClosure struct {
	Kind            string
	RequestID       uuid.UUID
	RequestDigest   string
	Body            json.RawMessage
	RunID           *uuid.UUID
	OriginalActorID *uuid.UUID
	ServiceActorID  uuid.UUID
	State           string
	Response        json.RawMessage
}

// LockObservationAdmissionTx is shared by both submission phases and recovery.
// It precedes grant/receipt locks so a committed closure cannot race a late send.
func LockObservationAdmissionTx(ctx context.Context, tx Tx, org, plugin, kind string, id uuid.UUID) error {
	if id == uuid.Nil || org == "" || !externalPluginIdentity.MatchString(plugin) || (kind != ObservationAdmission && kind != ObservationRedeploymentAdmission) {
		return NewErrBadRequest("exact observation admission identity required")
	}
	_, err := tx.ExecContext(ctx, `SELECT environment_observation_admission_lock($1,$2,$3,$4)`, org, plugin, kind, id)
	return err
}
func CheckObservationAdmissionOpenTx(ctx context.Context, tx Tx, org, plugin, kind string, id uuid.UUID) error {
	if err := LockObservationAdmissionTx(ctx, tx, org, plugin, kind, id); err != nil {
		return err
	}
	var closed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM environment_observation_closures WHERE org_id=$1 AND plugin_id=$2 AND kind=$3 AND request_id=$4)`, org, plugin, kind, id).Scan(&closed); err != nil {
		return err
	}
	if closed {
		return NewErrConflict("observation admission is permanently closed")
	}
	return nil
}
func ReadObservationClosureTx(ctx context.Context, tx Tx, org, plugin, kind string, id uuid.UUID) (*ObservationClosure, error) {
	c := &ObservationClosure{Kind: kind, RequestID: id}
	var actor, run uuid.NullUUID
	var response []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,request_body,run_id,original_actor_id,service_actor_id,state,response FROM environment_observation_closures WHERE org_id=$1 AND plugin_id=$2 AND kind=$3 AND request_id=$4 FOR UPDATE`, org, plugin, kind, id).Scan(&c.RequestDigest, &c.Body, &run, &actor, &c.ServiceActorID, &c.State, &response)
	if err != nil {
		return nil, err
	}
	if actor.Valid {
		c.OriginalActorID = &actor.UUID
	}
	if run.Valid {
		c.RunID = &run.UUID
	}
	c.Response = response
	return c, nil
}
func RecordObservationClosureTx(ctx context.Context, tx Tx, org, plugin string, c ObservationClosure) (*ObservationClosure, error) {
	if err := LockObservationAdmissionTx(ctx, tx, org, plugin, c.Kind, c.RequestID); err != nil {
		return nil, err
	}
	if c.ServiceActorID == uuid.Nil || len(c.RequestDigest) != 64 || len(c.Body) == 0 || c.Kind == ObservationRedeploymentAdmission && (c.RunID == nil || *c.RunID == uuid.Nil) || c.Kind == ObservationAdmission && c.RunID != nil {
		return nil, NewErrBadRequest("exact closure identity required")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_observation_closures(org_id,plugin_id,kind,request_id,request_digest,request_body,run_id,original_actor_id,service_actor_id,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'close_requested') ON CONFLICT DO NOTHING`, org, plugin, c.Kind, c.RequestID, c.RequestDigest, string(c.Body), c.RunID, c.OriginalActorID, c.ServiceActorID)
	if err != nil {
		return nil, err
	}
	stored, err := ReadObservationClosureTx(ctx, tx, org, plugin, c.Kind, c.RequestID)
	if err != nil {
		return nil, err
	}
	if stored.RequestDigest != c.RequestDigest || !equalObservationUUID(stored.RunID, c.RunID) {
		return nil, NewErrConflict("observation closure identifies another exact request")
	}
	return stored, nil
}
func equalObservationUUID(a, b *uuid.UUID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func CompleteObservationClosureTx(ctx context.Context, tx Tx, org, plugin, kind string, id uuid.UUID, state string, response json.RawMessage) error {
	if state != "accepted" && state != "not_started" {
		return NewErrBadRequest("terminal observation resolution required")
	}
	_, err := tx.ExecContext(ctx, `UPDATE environment_observation_closures SET state=$5,response=$6,resolved_at=clock_timestamp() WHERE org_id=$1 AND plugin_id=$2 AND kind=$3 AND request_id=$4 AND state='close_requested'`, org, plugin, kind, id, state, string(response))
	return err
}
