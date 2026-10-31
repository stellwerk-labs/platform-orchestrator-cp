package model

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

const (
	admissionStateActive        = "active"
	admissionStateSettled       = "settled"
	admissionModeDestroy        = "destroy"
	deploymentModeRollback      = "rollback"
	deploymentOutcomeCancelled  = "cancelled"
	deploymentOutcomeFailed     = "failed"
	deploymentOutcomeTerminated = "terminated"
	deploymentOutcomeAbsent     = "absent"
)

// DeploymentAdmission is a Core protection guard, not a second Deployment.
// DP owns execution and authenticates through the operator's service binding.
type DeploymentAdmission struct {
	OrgID                           string               `json:"org_id"`
	AdmissionID                     uuid.UUID            `json:"admission_id"`
	ProjectUUID                     uuid.UUID            `json:"project_uuid"`
	EnvironmentUUID                 uuid.UUID            `json:"environment_uuid"`
	ActorID                         uuid.UUID            `json:"actor_id"`
	RequestDigest                   string               `json:"request_digest"`
	Mode                            string               `json:"mode"`
	ModuleVersions                  map[string]uuid.UUID `json:"module_versions"`
	CarryForwardModuleVersions      map[string]uuid.UUID `json:"carry_forward_module_versions"`
	ConfirmedRestrictedVersionUUIDs []uuid.UUID          `json:"confirmed_restricted_version_uuids"`
	DraftTestSnapshots              map[string]uuid.UUID `json:"draft_test_snapshots,omitempty"`
}

type DeploymentAdmissionReceipt struct {
	AdmissionID     uuid.UUID `json:"admission_id"`
	ProjectUUID     uuid.UUID `json:"project_uuid"`
	EnvironmentUUID uuid.UUID `json:"environment_uuid"`
	RequestDigest   string    `json:"request_digest"`
	Epoch           int64     `json:"epoch"`
	State           string    `json:"state"`
	Mode            string    `json:"mode"`
	Outcome         string    `json:"outcome,omitempty"`
}

type DeploymentAdmissionPolicy struct {
	AllowFreshProposed   bool
	AllowFreshDraftTests bool
}

// ReadEnvironmentAdmissionEpoch must precede ALL remote evidence reads used to
// create or restore protection. Checking only unresolved guards misses an entire
// fast Deployment that starts and finishes during evidence collection.
func ReadEnvironmentAdmissionEpoch(ctx context.Context, db Databaser, tx Tx, org string, environment uuid.UUID) (int64, error) {
	if tx == nil {
		owned, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return 0, err
		}
		defer func() { _ = owned.Rollback() }()
		tx = owned
	}
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT epoch FROM environment_admission_epochs WHERE org_id=$1 AND environment_uuid=$2),0)`, org, environment).Scan(&epoch)
	return epoch, err
}

func CheckEnvironmentAdmissionEpoch(ctx context.Context, db Databaser, tx Tx, org string, environment uuid.UUID, expected int64) error {
	if tx == nil || expected < 0 {
		return NewErrBadRequest("transaction and captured admission epoch are required")
	}
	if err := lockExternalDispatchEnvironment(ctx, tx, environment); err != nil {
		return err
	}
	actual, err := ReadEnvironmentAdmissionEpoch(ctx, db, tx, org, environment)
	if err != nil {
		return err
	}
	if actual != expected {
		return NewErrConflict("Environment admission changed while inspecting effective state; refresh Pin evidence")
	}
	return checkExternalDispatchBarrier(ctx, tx, org, environment)
}

func advanceEnvironmentAdmissionEpoch(ctx context.Context, tx Tx, org string, environment uuid.UUID) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, `INSERT INTO environment_admission_epochs(org_id,environment_uuid,epoch) VALUES($1,$2,1)
        ON CONFLICT(org_id,environment_uuid) DO UPDATE SET epoch=environment_admission_epochs.epoch+1 RETURNING epoch`, org, environment).Scan(&epoch)
	return epoch, err
}

func validateDeploymentAdmission(value DeploymentAdmission) error {
	digest, err := hex.DecodeString(value.RequestDigest)
	if value.OrgID == "" || value.AdmissionID == uuid.Nil || value.ProjectUUID == uuid.Nil || value.EnvironmentUUID == uuid.Nil || value.ActorID == uuid.Nil || err != nil || len(digest) != 32 || strings.ToLower(value.RequestDigest) != value.RequestDigest || !slices.Contains([]string{"deploy", deploymentModeRollback, admissionModeDestroy}, value.Mode) || value.ModuleVersions == nil || len(value.ModuleVersions) > 1000 {
		return NewErrBadRequest("exact bounded Deployment admission identity and resolved Module Versions are required")
	}
	for key, version := range value.ModuleVersions {
		module, err := uuid.Parse(key)
		if err != nil || module == uuid.Nil || module.String() != key || version == uuid.Nil {
			return NewErrBadRequest("resolved Modules and Versions require canonical immutable UUIDs")
		}
	}
	if len(value.CarryForwardModuleVersions) > 1000 || len(value.ConfirmedRestrictedVersionUUIDs) > 1000 {
		return NewErrBadRequest("bounded carry-forward and restricted-version evidence is required")
	}
	for module, version := range value.CarryForwardModuleVersions {
		if target, found := value.ModuleVersions[module]; !found || target != version {
			return NewErrBadRequest("carry-forward evidence must be an exact subset of resolved Module Versions")
		}
	}
	if len(value.DraftTestSnapshots) > 1000 {
		return NewErrBadRequest("Draft test selection must be bounded")
	}
	for module, snapshot := range value.DraftTestSnapshots {
		if target, found := value.ModuleVersions[module]; !found || snapshot != target {
			return NewErrBadRequest("Draft test snapshots must be an exact subset of resolved Module targets")
		}
	}
	seenConfirmations := make(map[uuid.UUID]struct{}, len(value.ConfirmedRestrictedVersionUUIDs))
	for _, version := range value.ConfirmedRestrictedVersionUUIDs {
		if version == uuid.Nil {
			return NewErrBadRequest("restricted-version confirmations require immutable Version UUIDs")
		}
		if _, duplicate := seenConfirmations[version]; duplicate {
			return NewErrBadRequest("restricted-version confirmations must be unique")
		}
		seenConfirmations[version] = struct{}{}
	}
	if value.Mode == admissionModeDestroy && (len(value.ModuleVersions) != 0 || len(value.CarryForwardModuleVersions) != 0 || len(value.ConfirmedRestrictedVersionUUIDs) != 0) {
		return NewErrBadRequest("destroy admissions do not accept Module lifecycle evidence")
	}
	return nil
}

func canonicalDeploymentAdmission(value DeploymentAdmission) DeploymentAdmission {
	value.ModuleVersions = maps.Clone(value.ModuleVersions)
	if value.ModuleVersions == nil {
		value.ModuleVersions = map[string]uuid.UUID{}
	}
	value.CarryForwardModuleVersions = maps.Clone(value.CarryForwardModuleVersions)
	if value.CarryForwardModuleVersions == nil {
		value.CarryForwardModuleVersions = map[string]uuid.UUID{}
	}
	value.DraftTestSnapshots = maps.Clone(value.DraftTestSnapshots)
	if len(value.DraftTestSnapshots) == 0 {
		value.DraftTestSnapshots = nil
	}
	value.ConfirmedRestrictedVersionUUIDs = slices.Clone(value.ConfirmedRestrictedVersionUUIDs)
	if value.ConfirmedRestrictedVersionUUIDs == nil {
		value.ConfirmedRestrictedVersionUUIDs = []uuid.UUID{}
	}
	slices.SortFunc(value.ConfirmedRestrictedVersionUUIDs, func(a, b uuid.UUID) int {
		return strings.Compare(a.String(), b.String())
	})
	return value
}

func admissionReceipt(value DeploymentAdmission, epoch int64, outcome string) *DeploymentAdmissionReceipt {
	state := admissionStateActive
	if outcome != "" {
		state = admissionStateSettled
	}
	return &DeploymentAdmissionReceipt{AdmissionID: value.AdmissionID, ProjectUUID: value.ProjectUUID, EnvironmentUUID: value.EnvironmentUUID, RequestDigest: value.RequestDigest, Epoch: epoch, State: state, Mode: value.Mode, Outcome: outcome}
}

func AuthorizeDeploymentAdmission(ctx context.Context, db Databaser, value DeploymentAdmission, policies ...DeploymentAdmissionPolicy) (*DeploymentAdmissionReceipt, error) {
	if len(policies) > 1 {
		return nil, NewErrBadRequest("one trusted Deployment admission policy decision is required")
	}
	policy := DeploymentAdmissionPolicy{}
	if len(policies) == 1 {
		policy = policies[0]
	}
	value = canonicalDeploymentAdmission(value)
	if err := validateDeploymentAdmission(value); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalDispatchEnvironment(ctx, tx, value.EnvironmentUUID); err != nil {
		return nil, err
	}
	var raw []byte
	var epoch int64
	var outcome string
	var replay bool
	err = tx.QueryRowContext(ctx, `SELECT intent,epoch,COALESCE(outcome,'') FROM deployment_admission_guards WHERE org_id=$1 AND admission_id=$2`, value.OrgID, value.AdmissionID).Scan(&raw, &epoch, &outcome)
	if err == nil {
		var previous DeploymentAdmission
		if err := json.Unmarshal(raw, &previous); err != nil {
			return nil, err
		}
		previous = canonicalDeploymentAdmission(previous)
		if !reflect.DeepEqual(previous, value) || outcome != "" {
			return nil, NewErrConflict("admission identity was settled or belongs to a different exact request")
		}
		replay = true
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !replay {
		if err := checkExternalDispatchBarrier(ctx, tx, value.OrgID, value.EnvironmentUUID); err != nil {
			return nil, err
		}
	}
	environment, err := db.GetEnvironmentByUuid(ctx, tx, value.OrgID, value.EnvironmentUUID, GetModeDefault)
	if err != nil {
		return nil, err
	}
	if environment.ProjectUuid != value.ProjectUUID || value.Mode == admissionModeDestroy && environment.Status != EnvironmentStatusDeleting {
		return nil, NewErrConflict("admission requires the exact Environment scope and destroy requires confirmed deletion")
	}
	pins, err := db.ListEnvironmentModuleVersionPins(ctx, tx, value.OrgID, &value.EnvironmentUUID, nil, false)
	if err != nil {
		return nil, err
	}
	for _, pin := range pins {
		if pin.Status != moduleversions.PinActive && pin.Status != moduleversions.PinOverridePending {
			continue
		}
		if value.Mode == admissionModeDestroy && pin.Status == moduleversions.PinActive {
			continue
		}
		if value.Mode == admissionModeDestroy || value.ModuleVersions[pin.ModuleUUID.String()] != pin.VersionUUID {
			return nil, ErrConflict{Code: PinProtection, Message: "resolved Deployment does not preserve current Pin protection"}
		}
	}
	activePins := make(map[string]uuid.UUID)
	for _, pin := range pins {
		if pin.Status == moduleversions.PinActive {
			activePins[pin.ModuleUUID.String()] = pin.VersionUUID
		}
	}
	confirmed := make(map[uuid.UUID]struct{}, len(value.ConfirmedRestrictedVersionUUIDs))
	for _, version := range value.ConfirmedRestrictedVersionUUIDs {
		confirmed[version] = struct{}{}
	}
	for _, module := range slices.Sorted(maps.Keys(value.ModuleVersions)) {
		versionUUID := value.ModuleVersions[module]
		catalogue, err := db.GetModuleCatalogue(ctx, tx, value.OrgID, module, GetModeForUpdate)
		if err != nil {
			return nil, err
		}
		if snapshotUUID, selected := value.DraftTestSnapshots[module]; selected {
			snapshot, err := db.GetModuleDraftTestSnapshot(ctx, tx, value.OrgID, module, value.EnvironmentUUID, snapshotUUID)
			if err != nil {
				return nil, err
			}
			if snapshot.ProjectUUID != value.ProjectUUID {
				return nil, NewErrBadRequest("Draft test snapshot Project does not match admission scope")
			}
			retained := value.CarryForwardModuleVersions[module] == snapshotUUID
			if retained || value.Mode == deploymentModeRollback {
				var executed bool
				err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_admission_guards
     WHERE org_id=$1 AND environment_uuid=$2 AND outcome='succeeded'
       AND intent->'module_versions'->>$3=$4
       AND intent->'draft_test_snapshots'->>$3=$4)`, value.OrgID, value.EnvironmentUUID, module, snapshotUUID.String()).Scan(&executed)
				if err != nil {
					return nil, err
				}
				if !executed {
					return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "retained Draft tests require a successful prior admission in this Environment"}
				}
			}
			if catalogue.Status == moduleversions.CatalogueArchived && !retained && value.Mode != deploymentModeRollback {
				return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "archived Modules permit only exact retained test material"}
			}
			if !retained && value.Mode != deploymentModeRollback && !policy.AllowFreshDraftTests {
				return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "fresh Draft tests require current Module authoring authority"}
			}
			continue
		}
		version, err := db.GetCoreModuleVersion(ctx, tx, value.OrgID, module, versionUUID.String(), GetModeForUpdate)
		if err != nil {
			return nil, err
		}
		if catalogue.Status == moduleversions.CatalogueArchived && value.Mode != deploymentModeRollback && value.CarryForwardModuleVersions[module] != versionUUID {
			return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "archived Modules permit only exact carry-forward or rollback Versions"}
		}
		pinned := activePins[module] == versionUUID
		switch version.LifecycleStatus {
		case moduleversions.LifecycleProposed:
			if !pinned && value.Mode != deploymentModeRollback && value.CarryForwardModuleVersions[module] != versionUUID && !policy.AllowFreshProposed {
				return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "fresh Proposed Module Versions require current scoped authority"}
			}
		case moduleversions.LifecycleDeprecated:
			if !pinned && value.Mode != deploymentModeRollback {
				return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "Deprecated Module Versions require an active Pin or rollback"}
			}
		case moduleversions.LifecycleDefective:
			if _, ok := confirmed[versionUUID]; !ok || !pinned && value.Mode != deploymentModeRollback {
				return nil, ErrConflict{Code: ModuleVersionLifecycle, Message: "Defective Module Versions require exact confirmation and an active Pin or rollback"}
			}
			delete(confirmed, versionUUID)
		}
	}
	if len(confirmed) != 0 {
		return nil, NewErrBadRequest("restricted-version confirmations must name Defective target Versions")
	}
	if replay {
		return admissionReceipt(value, epoch, outcome), nil
	}
	epoch, err = advanceEnvironmentAdmissionEpoch(ctx, tx, value.OrgID, value.EnvironmentUUID)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deployment_admission_guards(org_id,admission_id,environment_uuid,project_uuid,intent,epoch) VALUES($1,$2,$3,$4,$5,$6)`, value.OrgID, value.AdmissionID, value.EnvironmentUUID, value.ProjectUUID, raw, epoch)
	if err != nil {
		if isUniqueViolation(err, "deployment_admission_guards_pkey") {
			return nil, NewErrConflict("admission identity was used concurrently in another Environment")
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return admissionReceipt(value, epoch, ""), nil
}

// SettleDeploymentAdmission accepts only a terminal result or a sealed absence
// receipt from the authenticated DP. Timeout/lease expiry are not outcomes. The
// API never delegates this authority to a plugin or a human caller.
func SettleDeploymentAdmission(ctx context.Context, db Databaser, org string, admission, project, environment uuid.UUID, digest, outcome string) (*DeploymentAdmissionReceipt, error) {
	if !slices.Contains([]string{externalDeploymentSucceeded, deploymentOutcomeFailed, deploymentOutcomeCancelled, deploymentOutcomeTerminated, deploymentOutcomeAbsent}, outcome) {
		return nil, NewErrBadRequest("an authoritative terminal admission outcome is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalDispatchEnvironment(ctx, tx, environment); err != nil {
		return nil, err
	}
	var raw []byte
	var epoch int64
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT intent,epoch,COALESCE(outcome,'') FROM deployment_admission_guards WHERE org_id=$1 AND admission_id=$2 FOR UPDATE`, org, admission).Scan(&raw, &epoch, &previous)
	if err != nil {
		return nil, err
	}
	var value DeploymentAdmission
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value.EnvironmentUUID != environment || value.ProjectUUID != project || value.RequestDigest != digest || previous != "" && previous != outcome {
		return nil, NewErrConflict("admission completion scope and outcome are immutable")
	}
	if previous == "" {
		_, err = tx.ExecContext(ctx, `UPDATE deployment_admission_guards SET outcome=$3,settled_at=now() WHERE org_id=$1 AND admission_id=$2`, org, admission, outcome)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return admissionReceipt(value, epoch, outcome), nil
}

// SealDeploymentAdmissionAbsence closes even a delayed authorize request. DP
// must first retain a permanent absent tombstone under its admission lock, after
// proving no Deployment committed. This never grants execution authority.
func SealDeploymentAdmissionAbsence(ctx context.Context, db Databaser, value DeploymentAdmission) (*DeploymentAdmissionReceipt, error) {
	value = canonicalDeploymentAdmission(value)
	if err := validateDeploymentAdmission(value); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalDispatchEnvironment(ctx, tx, value.EnvironmentUUID); err != nil {
		return nil, err
	}
	var raw []byte
	var epoch int64
	var outcome string
	err = tx.QueryRowContext(ctx, `SELECT intent,epoch,COALESCE(outcome,'') FROM deployment_admission_guards WHERE org_id=$1 AND admission_id=$2 FOR UPDATE`, value.OrgID, value.AdmissionID).Scan(&raw, &epoch, &outcome)
	if err == nil {
		var previous DeploymentAdmission
		if err := json.Unmarshal(raw, &previous); err != nil {
			return nil, err
		}
		previous = canonicalDeploymentAdmission(previous)
		if !reflect.DeepEqual(previous, value) || outcome != "" && outcome != deploymentOutcomeAbsent {
			return nil, NewErrConflict("admission absence scope, intent and outcome are immutable")
		}
		if outcome == "" {
			_, err = tx.ExecContext(ctx, `UPDATE deployment_admission_guards SET outcome=$3,settled_at=now() WHERE org_id=$1 AND admission_id=$2`, value.OrgID, value.AdmissionID, deploymentOutcomeAbsent)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		// Advancing the epoch is conservative: an in-flight Pin inspection may
		// retry, but it cannot mistake a crossed admission boundary for stability.
		epoch, err = advanceEnvironmentAdmissionEpoch(ctx, tx, value.OrgID, value.EnvironmentUUID)
		if err != nil {
			return nil, err
		}
		raw, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO deployment_admission_guards(org_id,admission_id,environment_uuid,project_uuid,intent,epoch,outcome,settled_at) VALUES($1,$2,$3,$4,$5,$6,$7,now())`, value.OrgID, value.AdmissionID, value.EnvironmentUUID, value.ProjectUUID, raw, epoch, deploymentOutcomeAbsent)
		if isUniqueViolation(err, "deployment_admission_guards_pkey") {
			return nil, NewErrConflict("admission identity was used concurrently in another Environment")
		}
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return admissionReceipt(value, epoch, deploymentOutcomeAbsent), nil
}
