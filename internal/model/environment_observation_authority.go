package model

import (
	"context"
	"github.com/google/uuid"
	"time"
)

// ReadObservationGrantMetadataTx holds the same grant row/advisory fence as
// metadata reads without loading the full selection into every admission.
func ReadObservationGrantMetadataTx(ctx context.Context, tx Tx, org, plugin string, id uuid.UUID) (*ObservationGrant, error) {
	if err := lockExternalGrant(ctx, tx, org, "observation:"+plugin, id); err != nil {
		return nil, err
	}
	grant := &ObservationGrant{OrgID: org, PluginID: plugin, GrantID: id}
	err := tx.QueryRowContext(ctx, `SELECT actor_id,source_operation,last_effect_request_id,revision,state,expires_at FROM environment_observation_grants WHERE org_id=$1 AND plugin_id=$2 AND grant_id=$3 FOR UPDATE`, org, plugin, id).Scan(&grant.ActorID, &grant.SourceOperation, &grant.LastEffectRequestID, &grant.Revision, &grant.State, &grant.ExpiresAt)
	return grant, err
}

// CheckIssuedObservationTargetTx is an indexed exact historical membership
// lookup. Rows are written atomically with immutable issued command receipts.
func CheckIssuedObservationTargetTx(ctx context.Context, tx Tx, org, plugin string, id uuid.UUID, revision int64, project, environment uuid.UUID) error {
	var member bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM environment_observation_grant_targets WHERE org_id=$1 AND plugin_id=$2 AND grant_id=$3 AND revision=$4 AND project_uuid=$5 AND environment_uuid=$6)`, org, plugin, id, revision, project, environment).Scan(&member)
	if err != nil {
		return err
	}
	if !member {
		return NewErrConflict("target exceeds exact issued observation scope")
	}
	return nil
}
func CheckObservationGrantTargetTx(ctx context.Context, tx Tx, grant *ObservationGrant, revision int64, project, environment uuid.UUID, now time.Time) error {
	if err := CheckObservationGrant(grant, revision, uuid.Nil, uuid.Nil, now); err != nil {
		return err
	}
	return CheckIssuedObservationTargetTx(ctx, tx, grant.OrgID, grant.PluginID, grant.GrantID, revision, project, environment)
}
