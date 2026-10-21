package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/google/uuid"
)

// ExternalDispatchCompletion is trusted DP receipt evidence, not plugin input.
// An absent receipt is conclusive only after withdrawing Core authority and
// waiting on DP's dispatch-key lock, which serializes callback through commit.
type ExternalDispatchCompletion struct {
	DeploymentID    uuid.UUID
	EnvironmentUUID uuid.UUID
	ProjectUUID     uuid.UUID
	RequestDigest   string
	Status          string
	NoEffect        bool
}

func WithdrawExternalDispatch(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID, dispatchID string) error {
	return updateExternalDispatchRecovery(ctx, db, org, plugin, operation, dispatchID, nil)
}

// RecoverWithdrawnExternalDispatch preserves the original request even if a
// failed Submit never recorded it. It never creates an active admission window.
// Current safety authority is fenced independently of the original intent.
func RecoverWithdrawnExternalDispatch(ctx context.Context, db Databaser, dispatch ExternalDispatch, currentFence int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalGrant(ctx, tx, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID); err != nil {
		return err
	}
	grant, err := readExternalGrant(ctx, tx, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID)
	if err != nil {
		return err
	}
	if grant.Revision != currentFence {
		return NewErrConflict("operation fence changed during dispatch recovery")
	}
	if err := validateExternalDispatchScope(grant, dispatch); err != nil {
		return err
	}
	if err := lockExternalDispatchEnvironment(ctx, tx, dispatch.EnvironmentUUID); err != nil {
		return err
	}
	var raw []byte
	var state string
	err = tx.QueryRowContext(ctx, `SELECT intent,state FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4 FOR UPDATE`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID).Scan(&raw, &state)
	if err == nil {
		var prior ExternalDispatch
		if err := json.Unmarshal(raw, &prior); err != nil {
			return err
		}
		if !reflect.DeepEqual(prior, dispatch) {
			return NewErrConflict("dispatch ID belongs to a different exact request")
		}
		if state == admissionStateActive {
			_, err = tx.ExecContext(ctx, `UPDATE external_operation_dispatches SET state='withdrawn',withdrawn_at=now() WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if dispatch.ActorID != grant.ActorID {
			return NewErrConflict("missing dispatch must bind the current authorized actor")
		}
		if err := validateExternalDispatchTargets(grant, dispatch); err != nil {
			return err
		}
		raw, err = json.Marshal(dispatch)
		if err != nil {
			return err
		}
		if _, err := advanceEnvironmentAdmissionEpoch(ctx, tx, dispatch.OrgID, dispatch.EnvironmentUUID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO external_operation_dispatches(org_id,plugin_id,operation_id,dispatch_id,fence,action,actor_id,request_digest,intent,state,withdrawn_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'withdrawn',now())`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, dispatch.Fence, dispatch.Action, dispatch.ActorID, dispatch.RequestDigest, raw)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteExternalDispatch runs only after the Core adapter checks authoritative
// terminal or no-effect evidence. It never renews a grant or retries execution.
func CompleteExternalDispatch(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID, dispatchID string, evidence ExternalDispatchCompletion) error {
	return updateExternalDispatchRecovery(ctx, db, org, plugin, operation, dispatchID, &evidence)
}

func updateExternalDispatchRecovery(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID, dispatchID string, evidence *ExternalDispatchCompletion) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalGrant(ctx, tx, org, plugin, operation); err != nil {
		return err
	}
	var encoded, previousCompletion []byte
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT intent,state,completion FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4 FOR UPDATE`, org, plugin, operation, dispatchID).Scan(&encoded, &state, &previousCompletion); err != nil {
		return err
	}
	var dispatch ExternalDispatch
	if err := json.Unmarshal(encoded, &dispatch); err != nil {
		return err
	}
	if err := lockExternalDispatchEnvironment(ctx, tx, dispatch.EnvironmentUUID); err != nil {
		return err
	}
	if evidence == nil {
		if state != admissionStateActive {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE external_operation_dispatches SET state='withdrawn',withdrawn_at=COALESCE(withdrawn_at,now()) WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, org, plugin, operation, dispatchID)
	} else {
		if state == admissionStateSettled {
			var prior ExternalDispatchCompletion
			if err := json.Unmarshal(previousCompletion, &prior); err != nil {
				return err
			}
			if !reflect.DeepEqual(prior, *evidence) {
				return NewErrConflict("dispatch completion is immutable")
			}
			return nil
		}
		if evidence.EnvironmentUUID != dispatch.EnvironmentUUID || evidence.ProjectUUID != dispatch.ProjectUUID || evidence.RequestDigest != dispatch.RequestDigest {
			return NewErrConflict("dispatch completion evidence belongs to another exact request")
		}
		if evidence.NoEffect {
			if state != "withdrawn" && state != admissionStateSettled || evidence.DeploymentID != uuid.Nil || evidence.Status != "" {
				return NewErrConflict("no-effect completion requires a withdrawn dispatch and authoritative absence")
			}
		} else if evidence.DeploymentID == uuid.Nil || !slices.Contains([]string{externalDeploymentSucceeded, deploymentOutcomeFailed, deploymentOutcomeCancelled, deploymentOutcomeTerminated}, evidence.Status) {
			return NewErrConflict("dispatch has no authoritative terminal Deployment outcome")
		}
		completion, encodeErr := json.Marshal(evidence)
		if encodeErr != nil {
			return encodeErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE external_operation_dispatches SET state='settled',settled_at=now(),completion=$5 WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, org, plugin, operation, dispatchID, completion)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
