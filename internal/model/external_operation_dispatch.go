package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"
)

// ExternalDispatch binds one remote effect to an exact, currently authorized
// grant generation. The digest covers the complete downstream request bytes.
// Its receipt is not permission to bypass current IAM, enablement or fencing.
type ExternalDispatch struct {
	OrgID           string    `json:"org_id"`
	PluginID        string    `json:"plugin_id"`
	OperationID     uuid.UUID `json:"operation_id"`
	Fence           int64     `json:"fence"`
	Action          string    `json:"action"`
	DispatchID      string    `json:"dispatch_id"`
	ProjectUUID     uuid.UUID `json:"project_uuid"`
	EnvironmentUUID uuid.UUID `json:"environment_uuid"`
	RequestDigest   string    `json:"request_digest"`
	ActorID         uuid.UUID `json:"actor_id"`
}

func validateExternalDispatch(grant *ExternalOperationGrant, dispatch ExternalDispatch) error {
	if grant.State != grantStateActive || grant.Revision != dispatch.Fence || grant.ActorID != dispatch.ActorID {
		return NewErrConflict("operation is not active at the requested actor and fence")
	}
	if !slices.Contains(grant.Intent.Actions, dispatch.Action) {
		return NewErrConflict("action is outside the frozen operation grant")
	}
	if dispatch.EnvironmentUUID == uuid.Nil || !slices.Contains(grant.Intent.Environments, ExternalOperationEnvironment{ProjectID: dispatch.ProjectUUID, EnvironmentID: dispatch.EnvironmentUUID}) {
		return NewErrConflict("environment is outside the frozen operation grant")
	}
	if dispatch.DispatchID == "" || len(dispatch.DispatchID) > 128 || len(dispatch.RequestDigest) != 64 {
		return NewErrBadRequest("dispatch identity and exact SHA256 request digest are required")
	}
	for _, ch := range dispatch.RequestDigest {
		if ch < '0' || ch > '9' && ch < 'a' || ch > 'f' {
			return NewErrBadRequest("request digest must be lowercase SHA256")
		}
	}
	return nil
}

// RecordExternalDispatch runs after current authorization, before contacting DP.
// The subsequent authenticated DP callback can only validate this recorded
// intent; it cannot manufacture a grant from an arbitrary request digest.
func RecordExternalDispatch(ctx context.Context, db Databaser, dispatch ExternalDispatch) error {
	return externalDispatchTransaction(ctx, db, dispatch, true)
}

func CheckExternalDispatch(ctx context.Context, db Databaser, dispatch ExternalDispatch) error {
	return externalDispatchTransaction(ctx, db, dispatch, false)
}

func externalDispatchTransaction(ctx context.Context, db Databaser, dispatch ExternalDispatch, create bool) error {
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
	if err := validateExternalDispatch(grant, dispatch); err != nil {
		return err
	}
	encoded, err := json.Marshal(dispatch)
	if err != nil {
		return err
	}
	var prior []byte
	err = tx.QueryRowContext(ctx, `SELECT intent FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID).Scan(&prior)
	if err == nil {
		var recorded ExternalDispatch
		if err := json.Unmarshal(prior, &recorded); err != nil {
			return err
		}
		if recorded != dispatch {
			return NewErrConflict("dispatch ID belongs to a different exact request")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !create {
		return NewErrConflict("dispatch was not authorized by Core")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_operation_dispatches(org_id,plugin_id,operation_id,dispatch_id,fence,action,actor_id,request_digest,intent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, dispatch.Fence, dispatch.Action, dispatch.ActorID, dispatch.RequestDigest, encoded)
	if err != nil {
		return err
	}
	return tx.Commit()
}
