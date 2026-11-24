package model

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
)

type NativeAdmissionWatermark struct {
	AdmissionID uuid.UUID `json:"admission_id"`
	Epoch       int64     `json:"epoch"`
}

var errNativeAdmissionHistory = ErrConflict{
	Code:    "native_admission_history_advanced",
	Message: "Native admission history requires reconciliation; refresh checked receipts or reconcile restored history before retrying",
}

// The caller holds existing Environment admission/row authority. This is
// admission knowledge, not the current provider-state head or raw epoch.
func checkNativeAdmissionHistory(ctx context.Context, tx Tx, env *Environment, value DeploymentAdmission) error {
	latest, err := readNativeAdmissionWatermark(ctx, tx, env, value.NativeTarget)
	if err != nil {
		return err
	}
	if latest != nil {
		if value.NativeAdmissionWatermark == nil || *value.NativeAdmissionWatermark != *latest {
			return errNativeAdmissionHistory
		}
		return checkNativeExternalHistory(ctx, tx, env, value.NativeTarget)
	}
	if value.NativeAdmissionWatermark != nil {
		return errNativeAdmissionHistory
	}
	var required bool
	if err := tx.QueryRowContext(ctx, `SELECT provider_state_required FROM envs WHERE org_id=$1 AND project_uuid=$2 AND uuid=$3`, env.OrgId, env.ProjectUuid, env.Uuid).Scan(&required); err != nil {
		return err
	}
	if required {
		return errNativeAdmissionHistory
	}
	return checkNativeExternalHistory(ctx, tx, env, value.NativeTarget)
}

func readNativeAdmissionWatermark(ctx context.Context, tx Tx, env *Environment, selected *NativeExecutionTargetReference) (*NativeAdmissionWatermark, error) {
	rows, err := tx.QueryContext(ctx, `SELECT admission_id,project_uuid,intent,epoch,outcome,settled_at
 FROM deployment_admission_guards WHERE org_id=$1 AND environment_uuid=$2 AND outcome IS DISTINCT FROM 'absent'
 ORDER BY epoch DESC LIMIT 2`, env.OrgId, env.Uuid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var latest *NativeAdmissionWatermark
	for rows.Next() {
		var id, project uuid.UUID
		var encoded []byte
		var epoch int64
		var outcome sql.NullString
		var settled sql.NullTime
		if err := rows.Scan(&id, &project, &encoded, &epoch, &outcome, &settled); err != nil {
			return nil, err
		}
		if latest != nil {
			if latest.Epoch == epoch {
				return nil, errNativeAdmissionHistory
			}
			continue
		}
		if !outcome.Valid {
			return nil, ErrConflict{Code: EnvironmentBusy, Message: "Native admission knowledge is still unsettled"}
		}
		if !settled.Valid || epoch <= 0 || project != env.ProjectUuid ||
			!validNativeAdmissionEvidence(encoded, id, env, selected) {
			return nil, errNativeAdmissionHistory
		}
		latest = &NativeAdmissionWatermark{AdmissionID: id, Epoch: epoch}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return latest, nil
}

func validNativeAdmissionEvidence(encoded []byte, id uuid.UUID, env *Environment, selected *NativeExecutionTargetReference) bool {
	var previous DeploymentAdmission
	if json.Unmarshal(encoded, &previous) != nil || validateDeploymentAdmission(previous) != nil || previous.NativeTarget == nil || selected == nil {
		return false
	}
	if previous.OrgID != env.OrgId || previous.ProjectUUID != env.ProjectUuid || previous.EnvironmentUUID != env.Uuid || previous.AdmissionID != id {
		return false
	}
	prior := previous.NativeTarget
	if prior.RunnerUUID != selected.RunnerUUID || prior.StateBackendType != selected.StateBackendType || prior.StateConfigurationDigest != selected.StateConfigurationDigest {
		return false
	}
	return true
}
