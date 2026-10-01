package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

// ExternalDispatch binds one remote effect to an exact, currently authorized
// grant generation. The digest covers the complete downstream request bytes.
// Its receipt is not permission to bypass current IAM, enablement or fencing.
type ExternalDispatch struct {
	OrgID           string                    `json:"org_id"`
	PluginID        string                    `json:"plugin_id"`
	OperationID     uuid.UUID                 `json:"operation_id"`
	Fence           int64                     `json:"fence"`
	Action          string                    `json:"action"`
	DispatchID      string                    `json:"dispatch_id"`
	ProjectUUID     uuid.UUID                 `json:"project_uuid"`
	EnvironmentUUID uuid.UUID                 `json:"environment_uuid"`
	RequestDigest   string                    `json:"request_digest"`
	ActorID         uuid.UUID                 `json:"actor_id"`
	TargetVersions  map[string]uuid.UUID      `json:"target_versions,omitempty"`
	ApprovedPins    map[string]uuid.UUID      `json:"approved_pins,omitempty"`
	Approval        *ExternalDispatchApproval `json:"approval,omitempty"`
}

// ExternalDispatchApproval identifies the original approver, not the controller
// or service identity. Core checks its operator-declared capability again at
// the DP admission callback, before any infrastructure execution begins.
type ExternalDispatchApproval struct {
	ActorID uuid.UUID `json:"actor_id"`
	Scope   string    `json:"scope"`
}

func GetExternalDispatch(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID, dispatchID string) (*ExternalDispatch, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT intent FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, org, plugin, operation, dispatchID).Scan(&raw); err != nil {
		return nil, err
	}
	var dispatch ExternalDispatch
	if err := json.Unmarshal(raw, &dispatch); err != nil {
		return nil, err
	}
	return &dispatch, nil
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
	if err := lockExternalDispatchEnvironment(ctx, tx, dispatch.EnvironmentUUID); err != nil {
		return err
	}
	encoded, err := json.Marshal(dispatch)
	if err != nil {
		return err
	}
	var prior []byte
	var state string
	err = tx.QueryRowContext(ctx, `SELECT intent,state FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID).Scan(&prior, &state)
	if err == nil {
		var recorded ExternalDispatch
		if err := json.Unmarshal(prior, &recorded); err != nil {
			return err
		}
		comparison := recorded
		if !create {
			comparison.TargetVersions, comparison.ApprovedPins = nil, nil
			comparison.Approval = nil
		}
		if !reflect.DeepEqual(comparison, dispatch) {
			return NewErrConflict("dispatch ID belongs to a different exact request")
		}
		if state != admissionStateActive {
			return NewErrConflict("dispatch is withdrawn or settled; inspect its receipt without retrying execution")
		}
		if err := validateExternalDispatchPins(ctx, db, tx, grant, recorded); err != nil {
			return err
		}
		// A callback or idempotent replay can also advance the entitlement
		// clock. Commit that decision at the exact-dispatch authorization point.
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !create {
		return NewErrConflict("dispatch was not authorized by Core")
	}
	if err := checkExternalDispatchBarrier(ctx, tx, dispatch.OrgID, dispatch.EnvironmentUUID); err != nil {
		return err
	}
	if err := validateExternalDispatchPins(ctx, db, tx, grant, dispatch); err != nil {
		return err
	}
	if _, err := advanceEnvironmentAdmissionEpoch(ctx, tx, dispatch.OrgID, dispatch.EnvironmentUUID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_operation_dispatches(org_id,plugin_id,operation_id,dispatch_id,fence,action,actor_id,request_digest,intent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, dispatch.OrgID, dispatch.PluginID, dispatch.OperationID, dispatch.DispatchID, dispatch.Fence, dispatch.Action, dispatch.ActorID, dispatch.RequestDigest, encoded)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func lockExternalDispatchEnvironment(ctx context.Context, tx Tx, environment uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 3))`, "external-dispatch-environment:"+environment.String())
	return err
}

func checkExternalDispatchBarrier(ctx context.Context, tx Tx, org string, environment uuid.UUID) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM external_operation_dispatches WHERE org_id=$1 AND intent->>'environment_uuid'=$2 AND state <> 'settled')`, org, environment.String()).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return NewErrConflict("Environment has an unresolved external Deployment dispatch")
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_admission_guards WHERE org_id=$1 AND environment_uuid=$2 AND outcome IS NULL)`, org, environment).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return NewErrConflict("Environment has an unresolved ordinary Deployment admission")
	}
	return nil
}

func validateExternalDispatchPins(ctx context.Context, db Databaser, tx Tx, grant *ExternalOperationGrant, dispatch ExternalDispatch) error {
	if len(dispatch.TargetVersions) == 0 || len(dispatch.TargetVersions) > 1000 {
		return NewErrBadRequest("dispatch requires exact Module target identities")
	}
	pins, err := db.ListEnvironmentModuleVersionPins(ctx, tx, dispatch.OrgID, &dispatch.EnvironmentUUID, nil, false)
	if err != nil {
		return err
	}
	boundaries := map[string]uuid.UUID{}
	if dispatch.ApprovedPins != nil {
		rows, err := tx.QueryContext(ctx, `SELECT p.module_uuid,p.activation_event_id FROM environment_module_version_pins p JOIN environment_module_version_pin_events e ON e.id=p.activation_event_id WHERE p.org_id=$1 AND p.environment_uuid=$2 ORDER BY e.sequence DESC`, dispatch.OrgID, dispatch.EnvironmentUUID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var module, activation uuid.UUID
			if err := rows.Scan(&module, &activation); err != nil {
				return err
			}
			if _, found := boundaries[module.String()]; !found {
				boundaries[module.String()] = activation
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for moduleRef, version := range dispatch.TargetVersions {
		module, err := uuid.Parse(moduleRef)
		if err != nil || module == uuid.Nil || version == uuid.Nil || !slices.Contains(grant.Intent.Modules, module) {
			return NewErrConflict("dispatch Module is outside the frozen grant")
		}
		if dispatch.Action == "external-deployment.admit" && grant.Intent.TargetVersions[moduleRef] != version {
			return NewErrConflict("dispatch target differs from the frozen Module Version")
		}
		if dispatch.ApprovedPins != nil {
			approved, covered := dispatch.ApprovedPins[moduleRef]
			if !covered || approved != boundaries[moduleRef] {
				return NewErrConflict("approval predates the latest Pin activation boundary")
			}
		}
		for _, pin := range pins {
			if pin.ModuleUUID != module || pin.Status == moduleversions.PinRemoved || pin.Status == moduleversions.PinOverridden {
				continue
			}
			if pin.VersionUUID == version {
				continue
			}
			if pin.Status != moduleversions.PinOverridePending || pin.OverrideOperationID == nil || *pin.OverrideOperationID != dispatch.OperationID || pin.OverrideTargetVersionUUID == nil || *pin.OverrideTargetVersionUUID != version {
				return NewErrConflict("dispatch is blocked by an active Pin or changed approval activation boundary")
			}
		}
	}
	return nil
}
