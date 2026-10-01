package model

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/plugincontext"
)

const (
	grantPrepare        = "prepare"
	grantActivate       = "activate"
	grantStatePrepared  = "prepared"
	grantStateActive    = "active"
	grantStateSuspended = "suspended"
	grantStateReleased  = "released"
)

// ExternalOperationIntent is the immutable bound of a delegated workflow, not
// a copy of the plugin's domain model. Core never interprets plugin statuses.
type ExternalOperationIntent struct {
	Fingerprint    string                         `json:"fingerprint"`
	Modules        []uuid.UUID                    `json:"modules"`
	TargetVersions map[string]uuid.UUID           `json:"target_versions"`
	Environments   []ExternalOperationEnvironment `json:"environments"`
	Actions        []string                       `json:"actions"`
}

type ExternalOperationEnvironment struct {
	ProjectID     uuid.UUID `json:"project_id"`
	EnvironmentID uuid.UUID `json:"environment_id"`
}

type ExternalOperationGrant struct {
	OrgID            string                  `json:"org_id"`
	PluginID         string                  `json:"plugin_id"`
	OperationID      uuid.UUID               `json:"operation_id"`
	ActorID          uuid.UUID               `json:"actor_id"`
	SourceOperation  string                  `json:"source_operation"`
	Intent           ExternalOperationIntent `json:"intent"`
	BaselineDefaults map[string]*uuid.UUID   `json:"baseline_defaults"`
	Revision         int64                   `json:"revision"`
	State            string                  `json:"state"`
	// SafetyActivation is Core-only authority from this exact revision's
	// immutable command receipt. It is never accepted or exposed by the API.
	SafetyActivation bool `json:"-"`
}

type externalGrantCommandReceipt struct {
	ExternalOperationGrant
	SafetyActivation bool `json:"safety_activation,omitempty"`
}

type ExternalGrantCommand struct {
	OrgID            string                   `json:"org_id"`
	PluginID         string                   `json:"plugin_id"`
	OperationID      uuid.UUID                `json:"operation_id"`
	ActorID          uuid.UUID                `json:"actor_id"`
	SourceOperation  string                   `json:"source_operation"`
	SourceDigest     string                   `json:"source_digest"`
	RequestID        string                   `json:"request_id"`
	ExpectedRevision int64                    `json:"expected_revision"`
	Action           string                   `json:"action"`
	Reason           string                   `json:"reason"`
	Intent           *ExternalOperationIntent `json:"intent,omitempty"`
	// Set only after Core verifies the original signed source command.
	SafetyActivation bool `json:"-"`
}

func validateExternalIntent(intent ExternalOperationIntent) error {
	if len(intent.Fingerprint) != 64 || strings.ContainsAny(intent.Fingerprint, "ABCDEF") || len(intent.Modules) == 0 || len(intent.Modules) > 1000 || len(intent.Environments) > 10000 || len(intent.Actions) == 0 || len(intent.Actions) > 64 {
		return NewErrBadRequest("bounded exact intent, modules and actions are required")
	}
	for _, ch := range intent.Fingerprint {
		if ch < '0' || ch > '9' && ch < 'a' || ch > 'f' {
			return NewErrBadRequest("intent fingerprint must be lowercase SHA256")
		}
	}
	modules := map[uuid.UUID]bool{}
	for _, id := range intent.Modules {
		if id == uuid.Nil || modules[id] {
			return NewErrBadRequest("module bounds must be unique immutable identities")
		}
		modules[id] = true
		if intent.TargetVersions[id.String()] == uuid.Nil {
			return NewErrBadRequest("every module needs an exact immutable target version")
		}
	}
	if len(intent.TargetVersions) != len(modules) {
		return NewErrBadRequest("target versions must match the frozen module set")
	}
	environments := map[uuid.UUID]bool{}
	for _, target := range intent.Environments {
		if target.ProjectID == uuid.Nil || target.EnvironmentID == uuid.Nil || environments[target.EnvironmentID] {
			return NewErrBadRequest("environment bounds must be unique immutable identities")
		}
		environments[target.EnvironmentID] = true
	}
	actions := map[string]bool{}
	for _, action := range intent.Actions {
		if action == "" || len(action) > 128 || actions[action] {
			return NewErrBadRequest("action bounds must be nonempty and unique")
		}
		actions[action] = true
	}
	return nil
}

func lockExternalGrant(ctx context.Context, tx Tx, org, plugin string, operation uuid.UUID) error {
	identity, _ := json.Marshal([]string{org, plugin, operation.String()})
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1))`, string(identity))
	return err
}

func readExternalGrant(ctx context.Context, tx Tx, org, plugin string, operation uuid.UUID) (*ExternalOperationGrant, error) {
	grant := &ExternalOperationGrant{OrgID: org, PluginID: plugin, OperationID: operation}
	var intent, defaults []byte
	err := tx.QueryRowContext(ctx, `SELECT actor_id,source_operation,intent,baseline_defaults,revision,state FROM external_operation_grants WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 FOR UPDATE`, org, plugin, operation).Scan(&grant.ActorID, &grant.SourceOperation, &intent, &defaults, &grant.Revision, &grant.State)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(intent, &grant.Intent); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(defaults, &grant.BaselineDefaults); err != nil {
		return nil, err
	}
	if grant.State != grantStateActive {
		return grant, nil
	}
	// The grant lock fences this receipt lookup against every activation. Old
	// or absent receipts carry no recovery authority. Historical receipts at a
	// different revision can never classify the current grant as safety.
	var response []byte
	err = tx.QueryRowContext(ctx, `SELECT response FROM external_operation_grant_commands
		WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 AND actor_id=$4
		AND response->>'revision'=$5 AND response->>'state'=$6`, org, plugin, operation, grant.ActorID, strconv.FormatInt(grant.Revision, 10), grant.State).Scan(&response)
	if errors.Is(err, sql.ErrNoRows) {
		return grant, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt externalGrantCommandReceipt
	if err := json.Unmarshal(response, &receipt); err != nil {
		return nil, err
	}
	if receipt.OrgID != org || receipt.PluginID != plugin || receipt.OperationID != operation || receipt.ActorID != grant.ActorID || receipt.Revision != grant.Revision || receipt.State != grant.State {
		return nil, NewErrConflict("operation grant command receipt identity changed")
	}
	grant.SafetyActivation = grant.State == grantStateActive && receipt.SafetyActivation
	return grant, nil
}

func GetExternalOperationGrant(ctx context.Context, db Databaser, org, plugin string, operation uuid.UUID) (*ExternalOperationGrant, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return readExternalGrant(ctx, tx, org, plugin, operation)
}

// ApplyExternalGrantCommand requires current service and actor authorization
// before every call, including replay. It persists only Core's bounded intent.
// Activation freshly authorizes the same immutable intent, even when already
// active. Every new activation binds its actor and rotates the grant fence;
// replay returns its original receipt without renewing current authority.
func ApplyExternalGrantCommand(ctx context.Context, db Databaser, command ExternalGrantCommand) (*ExternalOperationGrant, error) {
	if command.OrgID == "" || !externalPluginIdentity.MatchString(command.PluginID) || command.OperationID == uuid.Nil || command.ActorID == uuid.Nil || command.RequestID == "" || len(command.RequestID) > 128 || command.SourceOperation == "" || strings.TrimSpace(command.Reason) == "" {
		return nil, NewErrBadRequest("operation grant requires exact identity, request, source and reason")
	}
	if decoded, err := hex.DecodeString(command.SourceDigest); err != nil || len(decoded) != 32 || strings.ToLower(command.SourceDigest) != command.SourceDigest {
		return nil, NewErrBadRequest("exact source request digest is required")
	}
	if !slices.Contains([]string{grantPrepare, grantActivate, "suspend", "release"}, command.Action) {
		return nil, NewErrBadRequest("unsupported grant command")
	}
	if command.SafetyActivation && command.Action != grantActivate {
		return nil, NewErrBadRequest("only activation can retain verified safety authority")
	}
	if command.Action == grantPrepare {
		if command.Intent == nil || command.ExpectedRevision != 0 {
			return nil, NewErrBadRequest("prepare requires a new immutable intent")
		}
		if err := validateExternalIntent(*command.Intent); err != nil {
			return nil, err
		}
	} else if command.Intent != nil {
		return nil, NewErrBadRequest("prepared intent cannot be replaced")
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	digest := plugincontext.Digest(encoded)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockExternalGrant(ctx, tx, command.OrgID, command.PluginID, command.OperationID); err != nil {
		return nil, err
	}
	var priorDigest string
	var priorResponse []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response FROM external_operation_grant_commands WHERE org_id=$1 AND plugin_id=$2 AND (operation_id=$3 OR actor_id=$4) AND request_id=$5`, command.OrgID, command.PluginID, command.OperationID, command.ActorID, command.RequestID).Scan(&priorDigest, &priorResponse)
	if err == nil {
		if priorDigest != digest {
			return nil, NewErrConflict("grant request ID already belongs to another command or actor")
		}
		var previous externalGrantCommandReceipt
		if err := json.Unmarshal(priorResponse, &previous); err != nil {
			return nil, err
		}
		previous.ExternalOperationGrant.SafetyActivation = previous.State == grantStateActive && previous.SafetyActivation
		return &previous.ExternalOperationGrant, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	grant, err := readExternalGrant(ctx, tx, command.OrgID, command.PluginID, command.OperationID)
	if command.Action == grantPrepare {
		if err == nil {
			return nil, NewErrConflict("operation intent is already frozen")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		defaults := make(map[string]*uuid.UUID, len(command.Intent.Modules))
		moduleIDs := slices.Clone(command.Intent.Modules)
		slices.SortFunc(moduleIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		for _, moduleID := range moduleIDs {
			module, err := db.GetModuleCatalogue(ctx, tx, command.OrgID, moduleID.String(), GetModeForUpdate)
			if err != nil {
				return nil, err
			}
			defaults[moduleID.String()] = module.CurrentDefaultVersionUUID
			if _, err := db.GetCoreModuleVersion(ctx, tx, command.OrgID, moduleID.String(), command.Intent.TargetVersions[moduleID.String()].String(), GetModeDefault); err != nil {
				return nil, err
			}
		}
		for _, target := range command.Intent.Environments {
			environment, err := db.GetEnvironmentByUuid(ctx, tx, command.OrgID, target.EnvironmentID, GetModeDefault)
			if err != nil {
				return nil, err
			}
			if environment.ProjectUuid != target.ProjectID {
				return nil, NewErrBadRequest("environment does not belong to the frozen project")
			}
		}
		grant = &ExternalOperationGrant{OrgID: command.OrgID, PluginID: command.PluginID, OperationID: command.OperationID, ActorID: command.ActorID, SourceOperation: command.SourceOperation, Intent: *command.Intent, BaselineDefaults: defaults, Revision: 1, State: grantStatePrepared}
		intent, _ := json.Marshal(grant.Intent)
		baseline, _ := json.Marshal(grant.BaselineDefaults)
		_, err = tx.ExecContext(ctx, `INSERT INTO external_operation_grants(org_id,plugin_id,operation_id,actor_id,source_operation,intent,baseline_defaults,revision,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, grant.OrgID, grant.PluginID, grant.OperationID, grant.ActorID, grant.SourceOperation, intent, baseline, grant.Revision, grant.State)
	} else {
		if err != nil {
			return nil, err
		}
		if command.ExpectedRevision != grant.Revision {
			return nil, NewErrConflict("operation grant revision changed")
		}
		switch command.Action {
		case grantActivate:
			if !slices.Contains([]string{grantStatePrepared, grantStateSuspended, grantStateReleased, grantStateActive}, grant.State) {
				return nil, NewErrConflict("operation grant cannot be activated from current state")
			}
			grant.State = grantStateActive
			grant.ActorID = command.ActorID
		case "suspend":
			if grant.State != grantStateActive {
				return nil, NewErrConflict("only an active grant can be suspended")
			}
			grant.State = grantStateSuspended
		case "release":
			if grant.State == grantStateReleased {
				return nil, NewErrConflict("operation grant is already released")
			}
			grant.State = grantStateReleased
		}
		grant.Revision++
		_, err = tx.ExecContext(ctx, `UPDATE external_operation_grants SET state=$4,revision=$5,actor_id=$6,updated_at=now() WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3`, grant.OrgID, grant.PluginID, grant.OperationID, grant.State, grant.Revision, grant.ActorID)
	}
	if err != nil {
		return nil, err
	}
	grant.SafetyActivation = command.Action == grantActivate && command.SafetyActivation
	response, err := json.Marshal(externalGrantCommandReceipt{ExternalOperationGrant: *grant, SafetyActivation: grant.SafetyActivation})
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO external_operation_grant_commands(org_id,plugin_id,operation_id,request_id,actor_id,request_digest,reason,response) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, grant.OrgID, grant.PluginID, grant.OperationID, command.RequestID, command.ActorID, digest, command.Reason, response)
	if err != nil {
		var conflict *pq.Error
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			return nil, NewErrConflict("delegated request already authorized another operation or actor")
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return grant, nil
}
