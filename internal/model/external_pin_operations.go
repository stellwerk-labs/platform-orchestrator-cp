package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

const (
	externalPinBegin            = "begin"
	externalPinCancel           = "cancel"
	externalDeploymentSucceeded = "succeeded"
)

type ExternalPinTarget struct {
	PinID                   uuid.UUID `json:"pin_id"`
	ExpectedResourceVersion int64     `json:"expected_resource_version"`
	TargetVersionUUID       uuid.UUID `json:"target_version_uuid,omitempty"`
	DeploymentID            uuid.UUID `json:"deployment_id,omitempty"`
}

type ExternalPinCommand struct {
	OrgID       string              `json:"org_id"`
	PluginID    string              `json:"plugin_id"`
	OperationID uuid.UUID           `json:"operation_id"`
	GrantFence  int64               `json:"grant_fence"`
	ActorID     uuid.UUID           `json:"actor_id"`
	RequestID   string              `json:"request_id"`
	Action      string              `json:"action"`
	Reason      string              `json:"reason"`
	Targets     []ExternalPinTarget `json:"targets"`
}

// ExternalPinReceipt is a historical command result, not the current Pin state.
// Retaining the action allows recovery to reauthorize its exact service policy
// without retaining or replaying the original short-lived user proof.
type ExternalPinReceipt struct {
	OperationID uuid.UUID                     `json:"operation_id"`
	RequestID   string                        `json:"request_id"`
	Action      string                        `json:"action"`
	Pins        []EnvironmentModuleVersionPin `json:"pins"`
}

func externalPinCommandScope(plugin string, operation uuid.UUID) string {
	return "plugin-pin:" + plugin + ":" + operation.String()
}

// GetExternalPinReceipt serializes with command application but performs no
// transition. Absence is only a read result, never permission to repeat begin.
// The caller must authorize the returned grant's current controller and scope.
func GetExternalPinReceipt(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID, request string, fence int64) (*ExternalPinReceipt, *ExternalOperationGrant, error) {
	if org == "" || !externalPluginIdentity.MatchString(plugin) || operation == uuid.Nil || request == "" || len(request) > 128 || strings.ContainsAny(request, "\r\n") || fence <= 0 {
		return nil, nil, NewErrBadRequest("exact Pin receipt identity and current fence are required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalGrant(ctx, tx, org, plugin, operation); err != nil {
		return nil, nil, err
	}
	grant, err := readExternalGrant(ctx, tx, org, plugin, operation)
	if err != nil {
		return nil, nil, err
	}
	if grant.Revision != fence {
		return nil, nil, NewErrConflict("Pin receipt recovery requires the current operation fence")
	}
	_, prior, found, err := db.GetModuleCoreCommand(ctx, tx, org, externalPinCommandScope(plugin, operation), request)
	if err != nil || !found {
		return nil, grant, err
	}
	var receipt ExternalPinReceipt
	if err := json.Unmarshal(prior, &receipt); err != nil {
		return nil, nil, NewErrConflict("Pin receipt has no recoverable action metadata; explicit operator investigation is required")
	}
	if receipt.OperationID != operation || receipt.RequestID != request || !slices.Contains([]string{externalPinBegin, "reconcile", externalActionRestore, externalPinCancel}, receipt.Action) || !slices.Contains(grant.Intent.Actions, "pin.override."+receipt.Action) || len(receipt.Pins) == 0 || len(receipt.Pins) > 1000 {
		return nil, nil, NewErrConflict("Pin receipt does not match the frozen operation")
	}
	for _, pin := range receipt.Pins {
		if pin.ID == uuid.Nil || pin.OrgID != org || !slices.Contains(grant.Intent.Environments, ExternalOperationEnvironment{ProjectID: pin.ProjectUUID, EnvironmentID: pin.EnvironmentUUID}) || grant.Intent.TargetVersions[pin.ModuleUUID.String()] == uuid.Nil {
			return nil, nil, NewErrConflict("Pin receipt is outside the frozen operation scope")
		}
	}
	return &receipt, grant, nil
}

// ExternalPinEvidence is resolved by the trusted Core adapter, never decoded
// from a plugin request. Success includes the current effective version, so an
// intervening Deployment cannot be mistaken for completion of the override.
type ExternalPinEvidence struct {
	OperationID       uuid.UUID
	EnvironmentUUID   uuid.UUID
	ProjectUUID       uuid.UUID
	DeploymentID      uuid.UUID
	Status            string
	Mode              string
	ModuleVersions    map[uuid.UUID]uuid.UUID
	EffectiveVersions map[uuid.UUID]uuid.UUID
	AdmissionEpoch    int64
}

type ExternalPinEvidenceReader func(context.Context, Tx, *ExternalOperationGrant, EnvironmentModuleVersionPin, uuid.UUID) (ExternalPinEvidence, error)

// ApplyExternalPinCommand adds operation fencing and atomic bulk semantics to
// the existing Pin lifecycle. Callers authenticate the configured plugin,
// recheck current scoped IAM and verify a fresh explicit proof for begin.
// No direct Unpin or permanent discard action exists on this service port.
func ApplyExternalPinCommand(ctx context.Context, db Databaser, command ExternalPinCommand, evidenceReader ExternalPinEvidenceReader) ([]EnvironmentModuleVersionPin, error) {
	if command.OrgID == "" || !externalPluginIdentity.MatchString(command.PluginID) || command.OperationID == uuid.Nil || command.ActorID == uuid.Nil || command.ActorID.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" || command.GrantFence <= 0 || command.RequestID == "" || len(command.RequestID) > 128 || strings.ContainsAny(command.RequestID, "\r\n") || strings.TrimSpace(command.Reason) == "" || len(command.Targets) == 0 || len(command.Targets) > 1000 {
		return nil, NewErrBadRequest("exact bounded Pin command identity and targets are required")
	}
	if command.Action != externalPinBegin && command.Action != "reconcile" && command.Action != externalActionRestore && command.Action != externalPinCancel {
		return nil, NewErrBadRequest("unsupported operation-bound Pin action")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalGrant(ctx, tx, command.OrgID, command.PluginID, command.OperationID); err != nil {
		return nil, err
	}
	grant, err := readExternalGrant(ctx, tx, command.OrgID, command.PluginID, command.OperationID)
	if err != nil {
		return nil, err
	}
	if grant.Revision != command.GrantFence || !slices.Contains(grant.Intent.Actions, "pin.override."+command.Action) {
		return nil, NewErrConflict("Pin action is outside the current frozen operation grant")
	}
	if command.Action == externalPinBegin || command.Action == externalActionRestore {
		if grant.State != grantStateActive {
			return nil, NewErrConflict("Pin action requires an active operation")
		}
	}
	if command.Action != externalPinBegin && command.ActorID != grant.ActorID {
		return nil, NewErrConflict("Pin reconciliation requires the current controller")
	}
	targets := slices.Clone(command.Targets)
	slices.SortFunc(targets, func(a, b ExternalPinTarget) int { return strings.Compare(a.PinID.String(), b.PinID.String()) })
	pins := make([]EnvironmentModuleVersionPin, 0, len(targets))
	environmentLocks := []uuid.UUID{}
	for i, target := range targets {
		if target.PinID == uuid.Nil || target.ExpectedResourceVersion <= 0 || i > 0 && target.PinID == targets[i-1].PinID {
			return nil, NewErrBadRequest("Pin targets must be distinct exact identities and revisions")
		}
		pin, err := db.GetEnvironmentModuleVersionPin(ctx, tx, command.OrgID, target.PinID, GetModeDefault)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(grant.Intent.Environments, ExternalOperationEnvironment{ProjectID: pin.ProjectUUID, EnvironmentID: pin.EnvironmentUUID}) || grant.Intent.TargetVersions[pin.ModuleUUID.String()] == uuid.Nil {
			return nil, NewErrConflict("Pin is outside the frozen operation scope")
		}
		pins = append(pins, *pin)
		if !slices.Contains(environmentLocks, pin.EnvironmentUUID) {
			environmentLocks = append(environmentLocks, pin.EnvironmentUUID)
		}
	}
	slices.SortFunc(environmentLocks, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, environment := range environmentLocks {
		if err := lockExternalDispatchEnvironment(ctx, tx, environment); err != nil {
			return nil, err
		}
	}
	for i, target := range targets {
		pin, err := db.GetEnvironmentModuleVersionPin(ctx, tx, command.OrgID, target.PinID, GetModeForUpdate)
		if err != nil {
			return nil, err
		}
		pins[i] = *pin
	}
	command.Targets = targets
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	scope := externalPinCommandScope(command.PluginID, command.OperationID)
	priorFingerprint, prior, found, err := db.GetModuleCoreCommand(ctx, tx, command.OrgID, scope, command.RequestID)
	if err != nil {
		return nil, err
	}
	if found {
		if fingerprint != priorFingerprint {
			return nil, NewErrConflict("Pin request identity was used by another command or actor")
		}
		// Earlier experiment receipts contained only the Pin array. Exact
		// actor-bound replay remains compatible; read-only recovery requires
		// the action metadata of the new envelope and never guesses it.
		var result []EnvironmentModuleVersionPin
		if len(prior) > 0 && prior[0] == '[' {
			if err := json.Unmarshal(prior, &result); err != nil {
				return nil, err
			}
			return result, nil
		}
		var receipt ExternalPinReceipt
		if err := json.Unmarshal(prior, &receipt); err != nil {
			return nil, err
		}
		return receipt.Pins, nil
	}
	result := make([]EnvironmentModuleVersionPin, 0, len(pins))
	for i, pin := range pins {
		target := targets[i]
		next, event, version, deployment, err := externalPinDecision(ctx, db, tx, grant, command, pin, target, evidenceReader)
		if err != nil {
			return nil, err
		}
		updated, err := db.TransitionEnvironmentModuleVersionPin(ctx, tx, command.OrgID, pin.ID, target.ExpectedResourceVersion, next, event, command.ActorID, "addon", command.Reason, &command.OperationID, version, deployment)
		if err != nil {
			return nil, err
		}
		result = append(result, *updated)
	}
	receipt := ExternalPinReceipt{OperationID: command.OperationID, RequestID: command.RequestID, Action: command.Action, Pins: result}
	if err := db.StoreModuleCoreCommand(ctx, tx, command.OrgID, scope, command.RequestID, fingerprint, command.ActorID, receipt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func externalPinDecision(ctx context.Context, db Databaser, tx Tx, grant *ExternalOperationGrant, command ExternalPinCommand, pin EnvironmentModuleVersionPin, target ExternalPinTarget, evidenceReader ExternalPinEvidenceReader) (moduleversions.PinStatus, string, *uuid.UUID, *uuid.UUID, error) {
	deny := func(message string) (moduleversions.PinStatus, string, *uuid.UUID, *uuid.UUID, error) {
		return "", "", nil, nil, NewErrConflict(message)
	}
	if command.Action == externalPinBegin {
		if pin.Status != moduleversions.PinActive || target.TargetVersionUUID != grant.Intent.TargetVersions[pin.ModuleUUID.String()] || target.TargetVersionUUID == pin.VersionUUID || target.DeploymentID != uuid.Nil {
			return deny("override requires an active Pin and the exact different operation target")
		}
		module, err := db.GetModuleCatalogue(ctx, tx, command.OrgID, pin.ModuleUUID.String(), GetModeDefault)
		if err != nil {
			return "", "", nil, nil, err
		}
		version, err := db.GetCoreModuleVersion(ctx, tx, command.OrgID, pin.ModuleUUID.String(), target.TargetVersionUUID.String(), GetModeDefault)
		if err != nil {
			return "", "", nil, nil, err
		}
		if module.Status != moduleversions.CatalogueActive || version.LifecycleStatus != moduleversions.LifecycleProposed && version.LifecycleStatus != moduleversions.LifecycleDefault {
			return deny("override target requires an active Module and Proposed or Default version")
		}
		return moduleversions.PinOverridePending, "override_requested", &target.TargetVersionUUID, nil, nil
	}
	if pin.OverrideOperationID == nil || *pin.OverrideOperationID != command.OperationID || pin.Status == moduleversions.PinRemoved {
		return deny("Pin is not owned by this operation or was explicitly removed")
	}
	if command.Action == externalPinCancel {
		if pin.Status != moduleversions.PinOverridePending || target.DeploymentID != uuid.Nil || target.TargetVersionUUID != uuid.Nil {
			return deny("only an unstarted pending override may be cancelled")
		}
		var dispatched bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM external_operation_dispatches WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND intent->>'environment_uuid'=$4 AND NOT (state='settled' AND COALESCE(completion->>'NoEffect','false')='true'))`, command.OrgID, command.PluginID, command.OperationID, pin.EnvironmentUUID.String()).Scan(&dispatched); err != nil {
			return "", "", nil, nil, err
		}
		if dispatched {
			return deny("dispatch was recorded; reconcile its authoritative outcome before changing Pin protection")
		}
		return moduleversions.PinActive, "override_cancelled", nil, nil, nil
	}
	if evidenceReader == nil || target.DeploymentID == uuid.Nil || target.TargetVersionUUID != uuid.Nil {
		return deny("authoritative Deployment evidence is required")
	}
	evidence, err := evidenceReader(ctx, tx, grant, pin, target.DeploymentID)
	if err != nil {
		return "", "", nil, nil, err
	}
	if evidence.OperationID != command.OperationID || evidence.DeploymentID != target.DeploymentID || evidence.EnvironmentUUID != pin.EnvironmentUUID || evidence.ProjectUUID != pin.ProjectUUID {
		return deny("Deployment evidence belongs to another operation or Environment")
	}
	if evidence.Status == externalDeploymentSucceeded {
		if err := CheckEnvironmentAdmissionEpoch(ctx, db, tx, command.OrgID, pin.EnvironmentUUID, evidence.AdmissionEpoch); err != nil {
			return "", "", nil, nil, err
		}
	}
	if command.Action == externalActionRestore {
		if pin.Status != moduleversions.PinOverridden || evidence.Status != externalDeploymentSucceeded || evidence.Mode != deploymentModeRollback || evidence.ModuleVersions[pin.ModuleUUID] != pin.VersionUUID || evidence.EffectiveVersions[pin.ModuleUUID] != pin.VersionUUID {
			return deny("successful Rollback has not restored the exact pinned version")
		}
		return moduleversions.PinActive, "restored_after_rollback", nil, &target.DeploymentID, nil
	}
	if pin.Status != moduleversions.PinOverridePending || evidence.Mode != "deploy" {
		return deny("only the owning forward Deployment can reconcile a pending override")
	}
	switch evidence.Status {
	case externalDeploymentSucceeded:
		if pin.OverrideTargetVersionUUID == nil || evidence.ModuleVersions[pin.ModuleUUID] != *pin.OverrideTargetVersionUUID || evidence.EffectiveVersions[pin.ModuleUUID] != *pin.OverrideTargetVersionUUID {
			return deny("successful Deployment has not activated the exact override target")
		}
		return moduleversions.PinOverridden, "override_succeeded", pin.OverrideTargetVersionUUID, &target.DeploymentID, nil
	case deploymentOutcomeFailed, deploymentOutcomeCancelled, deploymentOutcomeTerminated:
		return moduleversions.PinActive, "override_failed", nil, &target.DeploymentID, nil
	default:
		return deny("Deployment outcome is not terminal")
	}
}
