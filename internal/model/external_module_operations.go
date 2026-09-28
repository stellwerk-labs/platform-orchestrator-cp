package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

type ExternalModuleTarget struct {
	ModuleUUID              uuid.UUID  `json:"module_uuid"`
	VersionUUID             uuid.UUID  `json:"version_uuid"`
	ExpectedDefaultUUID     *uuid.UUID `json:"expected_default_uuid"`
	ExpectedResourceVersion int64      `json:"expected_resource_version"`
}

type ExternalModuleCommand struct {
	OrgID         string                 `json:"org_id"`
	PluginID      string                 `json:"plugin_id"`
	OperationID   uuid.UUID              `json:"operation_id"`
	ActorID       uuid.UUID              `json:"actor_id"`
	RequestID     string                 `json:"request_id"`
	Action        string                 `json:"action"`
	ExpectedFence int64                  `json:"expected_fence"`
	Reason        string                 `json:"reason"`
	Targets       []ExternalModuleTarget `json:"targets"`
}

type ExternalModuleReceipt struct {
	OrgID       string      `json:"org_id"`
	PluginID    string      `json:"plugin_id"`
	OperationID uuid.UUID   `json:"operation_id"`
	RequestID   string      `json:"request_id"`
	Action      string      `json:"action"`
	Fence       int64       `json:"fence"`
	State       string      `json:"state"`
	ModuleUUIDs []uuid.UUID `json:"module_uuids"`
}

var externalPluginIdentity = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+$`)

const (
	externalActionReserve = "reserve"
	externalActionRelease = "release"
	externalActionPromote = "promote"
	externalActionRestore = "restore"
	externalStateActive   = "active"
	externalStateReleased = "released"
)

// ApplyExternalModuleOperation owns its Core transaction. Callers must verify
// service identity, declared grants, availability and actor scope BEFORE every
// call, including a receipt replay. A receipt is evidence, not authorization.
// Plugin state is committed separately using a durable request ID and reconciled
// by replay after interruption. No caller-owned remote transaction is implied.
func ApplyExternalModuleOperation(ctx context.Context, db Databaser, command ExternalModuleCommand) (*ExternalModuleReceipt, error) {
	if command.OrgID == "" || !externalPluginIdentity.MatchString(command.PluginID) || command.OperationID == uuid.Nil || command.ActorID == uuid.Nil || command.RequestID == "" || len(command.RequestID) > 128 || strings.TrimSpace(command.Reason) == "" || len(command.Targets) == 0 {
		return nil, NewErrBadRequest("external Module operation requires identity, reason and exact targets")
	}
	if !slices.Contains([]string{externalActionReserve, externalActionRelease, externalActionPromote, externalActionRestore}, command.Action) {
		return nil, NewErrBadRequest("unknown external Module operation")
	}
	command.Targets = slices.Clone(command.Targets)
	slices.SortFunc(command.Targets, func(a, b ExternalModuleTarget) int {
		return strings.Compare(a.ModuleUUID.String(), b.ModuleUUID.String())
	})
	ids := make([]uuid.UUID, len(command.Targets))
	textIDs := make([]string, len(command.Targets))
	for i, target := range command.Targets {
		if target.ModuleUUID == uuid.Nil || i > 0 && target.ModuleUUID == command.Targets[i-1].ModuleUUID {
			return nil, NewErrBadRequest("exact Module targets must be nonempty and unique")
		}
		ids[i] = target.ModuleUUID
		textIDs[i] = target.ModuleUUID.String()
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize an absent operation too. JSON tuple encoding avoids ambiguous
	// concatenation; a hash collision can only serialize unrelated operations.
	lockIdentity, _ := json.Marshal([]string{command.OrgID, command.PluginID, command.OperationID.String()})
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(lockIdentity)); err != nil {
		return nil, err
	}
	var storedDigest string
	var storedActor uuid.UUID
	var responseJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest, actor_id, response FROM external_module_operation_receipts WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND request_id=$4`, command.OrgID, command.PluginID, command.OperationID, command.RequestID).Scan(&storedDigest, &storedActor, &responseJSON)
	if err == nil {
		if storedDigest != digest || storedActor != command.ActorID {
			return nil, NewErrConflict("external operation request ID was used for a different command or actor")
		}
		var result ExternalModuleReceipt
		if err := json.Unmarshal(responseJSON, &result); err != nil {
			return nil, err
		}
		return &result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var fence int64
	var state string
	var storedIDs pq.StringArray
	err = tx.QueryRowContext(ctx, `SELECT fence,state,module_uuids FROM external_module_operations WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 FOR UPDATE`, command.OrgID, command.PluginID, command.OperationID).Scan(&fence, &state, &storedIDs)
	if errors.Is(err, sql.ErrNoRows) {
		if command.Action != externalActionReserve || command.ExpectedFence != 0 {
			return nil, NewErrConflict("operation must acquire its initial reservation first")
		}
		fence = 1
		state = externalStateReleased
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_module_operations (org_id,plugin_id,operation_id,fence,state,module_uuids) VALUES ($1,$2,$3,$4,'released',$5)`, command.OrgID, command.PluginID, command.OperationID, fence, pq.Array(textIDs)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if command.ExpectedFence != fence {
			return nil, NewErrConflict("external Module operation fence changed")
		}
		if !slices.Equal([]string(storedIDs), textIDs) {
			return nil, NewErrConflict("operation target membership is immutable")
		}
		if command.Action == externalActionReserve {
			if state != externalStateReleased {
				return nil, NewErrConflict("operation already holds its reservations")
			}
			fence++
		}
	}
	if command.Action != externalActionReserve && state != externalStateActive {
		return nil, NewErrConflict("operation does not hold active reservations")
	}
	// Lock exact UUIDs in canonical order for every action, including promotion.
	for _, target := range command.Targets {
		module, err := db.GetModuleCatalogue(ctx, tx, command.OrgID, target.ModuleUUID.String(), GetModeForUpdate)
		if err != nil {
			return nil, err
		}
		reservationOperationID := uuid.NewSHA1(command.OperationID, []byte(fmt.Sprintf("%s/%s/%d", command.PluginID, target.ModuleUUID, fence)))
		if command.Action == externalActionReserve {
			if _, err := db.AcquireModuleOperationReservation(ctx, tx, command.OrgID, target.ModuleUUID.String(), command.PluginID, reservationOperationID, command.OperationID.String(), command.Reason, command.ActorID); err != nil {
				return nil, err
			}
			continue
		}
		reservations, err := db.ListModuleOperationReservations(ctx, tx, command.OrgID, target.ModuleUUID, false)
		if err != nil {
			return nil, err
		}
		index := slices.IndexFunc(reservations, func(r ModuleOperationReservation) bool {
			return r.Namespace == command.PluginID && r.OperationID == reservationOperationID && r.RelatedResourceID == command.OperationID.String()
		})
		if index < 0 {
			return nil, NewErrConflict("operation reservation ownership changed")
		}
		if command.Action == externalActionRelease {
			reservation := reservations[index]
			if _, err := db.ReleaseModuleOperationReservation(ctx, tx, command.OrgID, reservation.ID, reservation.ResourceVersion, command.ActorID, command.Reason); err != nil {
				return nil, err
			}
			continue
		}
		if !sameOptionalUUID(module.CurrentDefaultVersionUUID, target.ExpectedDefaultUUID) {
			return nil, NewErrConflict("Module Default changed since the operation was prepared")
		}
		version, err := db.GetCoreModuleVersion(ctx, tx, command.OrgID, target.ModuleUUID.String(), target.VersionUUID.String(), GetModeForUpdate)
		if err != nil {
			return nil, err
		}
		eligible := moduleversions.LifecycleProposed
		if command.Action == externalActionRestore {
			eligible = moduleversions.LifecycleDeprecated
		}
		if version.LifecycleStatus != eligible {
			return nil, NewErrConflict("Module Version is not eligible for this operation")
		}
		if _, err := db.TransitionCoreModuleVersion(ctx, tx, command.OrgID, target.ModuleUUID.String(), target.VersionUUID.String(), moduleversions.LifecycleDefault, target.ExpectedResourceVersion, command.ActorID, command.Reason, &command.OperationID); err != nil {
			return nil, err
		}
	}
	switch command.Action {
	case externalActionReserve:
		state = externalStateActive
	case externalActionRelease:
		state = externalStateReleased
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_module_operations SET fence=$4,state=$5,updated_at=now() WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3`, command.OrgID, command.PluginID, command.OperationID, fence, state); err != nil {
		return nil, err
	}
	result := ExternalModuleReceipt{OrgID: command.OrgID, PluginID: command.PluginID, OperationID: command.OperationID, RequestID: command.RequestID, Action: command.Action, Fence: fence, State: state, ModuleUUIDs: ids}
	responseJSON, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_module_operation_receipts (org_id,plugin_id,operation_id,request_id,request_digest,actor_id,action,response) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, command.OrgID, command.PluginID, command.OperationID, command.RequestID, digest, command.ActorID, command.Action, responseJSON); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &result, nil
}

func sameOptionalUUID(a, b *uuid.UUID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
