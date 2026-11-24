package model

import (
	"context"
	"encoding/json"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

// The ordinary guard may replace only its own verified external dispatch reservation.
func checkOrdinaryExternalDispatchBarrier(ctx context.Context, db Databaser, tx Tx, value DeploymentAdmission, accepted bool) error {
	if value.ExternalDispatch == nil {
		return checkExternalDispatchBarrier(ctx, tx, value.OrgID, value.EnvironmentUUID)
	}
	owner := value.ExternalDispatch
	var encoded []byte

	var state string
	if err := tx.QueryRowContext(ctx, `SELECT intent,state FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND dispatch_id=$4 FOR UPDATE`, value.OrgID, owner.PluginID, owner.OperationID, owner.DispatchID).Scan(&encoded, &state); err != nil {
		return err
	}
	var recorded ExternalDispatch
	if err := json.Unmarshal(encoded, &recorded); err != nil {
		return err
	}
	if state != admissionStateActive || !externalAdmissionMatches(value, recorded) {
		return NewErrConflict("ordinary admission does not match its exact external dispatch")
	}
	grant, err := readExternalGrant(ctx, tx, value.OrgID, owner.PluginID, owner.OperationID)
	if err != nil {
		return err
	}
	if err := validateExternalAdmissionGrant(grant, recorded, accepted); err != nil {
		return err
	}
	if err := validateExternalDispatchPins(ctx, db, tx, nil, recorded); err != nil {
		return err
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM external_operation_dispatches WHERE org_id=$1 AND intent->>'environment_uuid'=$2 AND state<>'settled'
 AND NOT(plugin_id=$3 AND operation_id=$4 AND dispatch_id=$5)) OR EXISTS(SELECT 1 FROM deployment_admission_guards WHERE org_id=$1 AND environment_uuid=$6 AND outcome IS NULL AND admission_id<>$7)`, value.OrgID, value.EnvironmentUUID.String(), owner.PluginID, owner.OperationID, owner.DispatchID, value.EnvironmentUUID, value.AdmissionID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return NewErrConflict("Environment has another unresolved deployment admission")
	}
	return nil
}
func externalAdmissionMatches(value DeploymentAdmission, recorded ExternalDispatch) bool {
	owner := value.ExternalDispatch
	if owner == nil {
		return false
	}
	expected := ExternalAdmissionDispatch{PluginID: recorded.PluginID, OperationID: recorded.OperationID, DispatchID: recorded.DispatchID, Fence: recorded.Fence, RequestDigest: recorded.RequestDigest}
	if *owner != expected || recorded.OrgID != value.OrgID || recorded.ActorID != value.ActorID || recorded.ProjectUUID != value.ProjectUUID || recorded.EnvironmentUUID != value.EnvironmentUUID {
		return false
	}
	action := externalDeploymentAdmitAction
	if value.Mode == deploymentModeRollback {
		action = externalDeploymentRollbackAction
	} else if value.Mode != deploymentModeDeploy {
		return false
	}
	if recorded.Action != action {
		return false
	}
	for module, version := range recorded.TargetVersions {
		if value.ModuleVersions[module] != version {
			return false
		}
	}
	return externalAdmissionPipelineMatches(value.Pipeline, recorded.Pipeline)
}
func externalAdmissionPipelineMatches(admission *PipelineAdmissionReference, reference *p.PipelineReference) bool {
	if reference == nil {
		return admission == nil
	}
	if p.ValidateReference(*reference) != nil {
		return false
	}
	if reference.Builtin != nil {
		return admission == nil
	}
	return admission != nil && admission.VersionID == *reference.VersionId && admission.Digest == reference.Digest
}
func externalAdmissionOwnsPendingPin(value DeploymentAdmission, pin EnvironmentModuleVersionPin) bool {
	return value.ExternalDispatch != nil && value.Mode == deploymentModeDeploy && pin.Status == moduleversions.PinOverridePending && pin.OverrideOperationID != nil && *pin.OverrideOperationID == value.ExternalDispatch.OperationID && pin.OverrideTargetVersionUUID != nil && value.ModuleVersions[pin.ModuleUUID.String()] == *pin.OverrideTargetVersionUUID
}
func validateExternalAdmissionGrant(grant *ExternalOperationGrant, recorded ExternalDispatch, accepted bool) error {
	if !accepted {
		return validateExternalDispatch(grant, recorded)
	}
	if grant.State != grantStateActive && grant.State != grantStateSuspended {
		return NewErrConflict("external operation does not authorize this admission boundary")
	}
	return validateExternalDispatchScope(grant, recorded)
}
