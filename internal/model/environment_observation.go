package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

const (
	observationGrantReplace = "replace"
	observationGrantRevoke  = "revoke"
	observationGrantActive  = "active"
	observationGrantRevoked = "revoked"
)

// Observation authority has no Module or mutation scope and is independent of
// frozen-version operation grants. Replacing any selection rotates its fence.
type ObservationGrant struct {
	LastEffectRequestID string                         `json:"last_effect_request_id"`
	OrgID               string                         `json:"org_id"`
	PluginID            string                         `json:"plugin_id"`
	GrantID             uuid.UUID                      `json:"grant_id"`
	ActorID             uuid.UUID                      `json:"actor_id"`
	SourceOperation     string                         `json:"source_operation"`
	Revision            int64                          `json:"revision"`
	State               string                         `json:"state"`
	Targets             []ExternalOperationEnvironment `json:"targets"`
	ExpiresAt           *time.Time                     `json:"expires_at,omitempty"`
	BaselinePolicy      string                         `json:"baseline_policy"`
	ExecutionPolicy     string                         `json:"execution_policy"`
}

type ObservationGrantCommand struct {
	OrgID            string                         `json:"org_id"`
	PluginID         string                         `json:"plugin_id"`
	GrantID          uuid.UUID                      `json:"grant_id"`
	ActorID          uuid.UUID                      `json:"actor_id"`
	SourceOperation  string                         `json:"source_operation"`
	SourceDigest     string                         `json:"source_digest"`
	RequestID        string                         `json:"request_id"`
	ExpectedRevision int64                          `json:"expected_revision"`
	Action           string                         `json:"action"`
	Reason           string                         `json:"reason"`
	Targets          []ExternalOperationEnvironment `json:"targets"`
	ExpiresAt        *time.Time                     `json:"expires_at,omitempty"`
}

func ReadObservationGrantTx(ctx context.Context, tx Tx, org, plugin string, id uuid.UUID) (*ObservationGrant, error) {
	if err := lockExternalGrant(ctx, tx, org, "observation:"+plugin, id); err != nil {
		return nil, err
	}
	grant := &ObservationGrant{OrgID: org, PluginID: plugin, GrantID: id, BaselinePolicy: "last-successful-ordinary-deployment", ExecutionPolicy: "full-plan-if-idle"}
	var targets []byte
	err := tx.QueryRowContext(ctx, `SELECT actor_id,source_operation,last_effect_request_id,revision,state,targets,expires_at FROM environment_observation_grants WHERE org_id=$1 AND plugin_id=$2 AND grant_id=$3 FOR UPDATE`, org, plugin, id).Scan(&grant.ActorID, &grant.SourceOperation, &grant.LastEffectRequestID, &grant.Revision, &grant.State, &targets, &grant.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(targets, &grant.Targets); err != nil {
		return nil, err
	}
	return grant, nil
}

// ApplyObservationGrantCommandTx assumes fresh delegated actor, installation and
// entitlement checks in this same transaction. Receipt replay never renews scope.
func ApplyObservationGrantCommandTx(ctx context.Context, tx Tx, command ObservationGrantCommand, now time.Time) (*ObservationGrant, error) {
	if command.OrgID == "" || !externalPluginIdentity.MatchString(command.PluginID) || command.GrantID == uuid.Nil || command.ActorID == uuid.Nil || command.RequestID == "" || len(command.RequestID) > 128 || command.SourceOperation == "" || len(command.SourceDigest) != 64 || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 1024 || !slices.Contains([]string{observationGrantReplace, observationGrantRevoke}, command.Action) {
		return nil, NewErrBadRequest("exact observation authority and reason required")
	}
	if len(command.Targets) > 10000 || command.Action == observationGrantReplace && len(command.Targets) == 0 || command.Action == observationGrantRevoke && len(command.Targets) != 0 {
		return nil, NewErrBadRequest("replace needs explicit targets; revoke has no targets")
	}
	if command.ExpiresAt != nil && !command.ExpiresAt.After(now) {
		return nil, NewErrBadRequest("observation authority expiry must be in the future")
	}
	grant, err := ReadObservationGrantTx(ctx, tx, command.OrgID, command.PluginID, command.GrantID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	digest := plugincontext.Digest(encoded)
	var storedDigest string
	var storedActor uuid.UUID
	var response []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,actor_id,response FROM environment_observation_grant_commands WHERE org_id=$1 AND plugin_id=$2 AND grant_id=$3 AND request_id=$4`, command.OrgID, command.PluginID, command.GrantID, command.RequestID).Scan(&storedDigest, &storedActor, &response)
	if err == nil {
		if storedDigest != digest || storedActor != command.ActorID {
			return nil, NewErrConflict("observation request identifies another command or actor")
		}
		var replay ObservationGrant
		if err := json.Unmarshal(response, &replay); err != nil {
			return nil, err
		}
		return &replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if grant == nil {
		if command.ExpectedRevision != 0 {
			return nil, NewErrConflict("new observation grant requires revision zero")
		}
		grant = &ObservationGrant{OrgID: command.OrgID, PluginID: command.PluginID, GrantID: command.GrantID, BaselinePolicy: "last-successful-ordinary-deployment", ExecutionPolicy: "full-plan-if-idle"}
	} else if grant.Revision != command.ExpectedRevision {
		return nil, NewErrConflict("observation grant revision changed")
	}
	seen := map[uuid.UUID]bool{}
	projects := make([]string, 0, len(command.Targets))
	environments := make([]string, 0, len(command.Targets))
	for _, target := range command.Targets {
		if target.ProjectID == uuid.Nil || target.EnvironmentID == uuid.Nil || seen[target.EnvironmentID] {
			return nil, NewErrBadRequest("observation targets must be unique immutable identities")
		}
		seen[target.EnvironmentID] = true
		projects = append(projects, target.ProjectID.String())
		environments = append(environments, target.EnvironmentID.String())
	}
	if len(command.Targets) > 0 {
		var missing, changed int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE e.uuid IS NULL),count(*) FILTER(WHERE e.uuid IS NOT NULL AND e.project_uuid<>t.project_uuid) FROM unnest($2::uuid[],$3::uuid[]) t(project_uuid,environment_uuid) LEFT JOIN envs e ON e.org_id=$1 AND e.uuid=t.environment_uuid`, command.OrgID, pq.Array(projects), pq.Array(environments)).Scan(&missing, &changed)
		if err != nil {
			return nil, err
		}
		if missing > 0 {
			return nil, NewErrNotFound("observation target Environment not found")
		}
		if changed > 0 {
			return nil, NewErrConflict("observation target project changed")
		}
	}
	grant.LastEffectRequestID = command.RequestID
	grant.ActorID = command.ActorID
	grant.SourceOperation = command.SourceOperation
	grant.Revision++
	grant.State = observationGrantActive
	grant.Targets = command.Targets
	grant.ExpiresAt = command.ExpiresAt
	if command.Action == observationGrantRevoke {
		grant.State = observationGrantRevoked
		grant.Targets = []ExternalOperationEnvironment{}
		grant.ExpiresAt = nil
	}
	targets, _ := json.Marshal(grant.Targets)
	_, err = tx.ExecContext(ctx, `INSERT INTO environment_observation_grants(org_id,plugin_id,grant_id,actor_id,source_operation,revision,state,targets,expires_at,last_effect_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(org_id,plugin_id,grant_id) DO UPDATE SET actor_id=$4,source_operation=$5,revision=$6,state=$7,targets=$8,expires_at=$9,last_effect_request_id=$10,updated_at=now()`, grant.OrgID, grant.PluginID, grant.GrantID, grant.ActorID, grant.SourceOperation, grant.Revision, grant.State, string(targets), grant.ExpiresAt, grant.LastEffectRequestID)
	if err != nil {
		return nil, err
	}
	if grant.State == observationGrantActive {
		if _, err := tx.ExecContext(ctx, `INSERT INTO environment_observation_grant_targets(org_id,plugin_id,grant_id,revision,project_uuid,environment_uuid) SELECT $1,$2,$3,$4,t.project_uuid,t.environment_uuid FROM unnest($5::uuid[],$6::uuid[]) t(project_uuid,environment_uuid)`, grant.OrgID, grant.PluginID, grant.GrantID, grant.Revision, pq.Array(projects), pq.Array(environments)); err != nil {
			return nil, err
		}
	}
	response, _ = json.Marshal(grant)
	_, err = tx.ExecContext(ctx, `INSERT INTO environment_observation_grant_commands(org_id,plugin_id,grant_id,request_id,actor_id,request_digest,reason,response) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, command.OrgID, command.PluginID, command.GrantID, command.RequestID, command.ActorID, digest, command.Reason, string(response))
	if err != nil {
		var conflict *pq.Error
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			return nil, NewErrConflict("original delegated observation command was already used")
		}
		return nil, err
	}
	return grant, nil
}

func CheckObservationGrant(grant *ObservationGrant, revision int64, project, environment uuid.UUID, now time.Time) error {
	if grant.State != observationGrantActive || grant.Revision != revision || grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
		return NewErrConflict("observation authority is revoked, expired or superseded")
	}
	if project == uuid.Nil && environment == uuid.Nil {
		return nil
	}
	if !slices.ContainsFunc(grant.Targets, func(t ExternalOperationEnvironment) bool {
		return t.ProjectID == project && t.EnvironmentID == environment
	}) {
		return NewErrConflict("target exceeds exact observation scope")
	}
	return nil
}

type ObservationRequestRecord struct {
	ActorID         uuid.UUID
	GrantID         uuid.UUID
	GrantRevision   int64
	ProjectUUID     uuid.UUID
	EnvironmentUUID uuid.UUID
	RequestID       uuid.UUID
	RequestDigest   string
	Body            json.RawMessage
	Deadline        time.Time
	Response        json.RawMessage
	ResponseStatus  *int
	RunID           *uuid.UUID
}

func ReadObservationRequestTx(ctx context.Context, tx Tx, org, plugin string, requestID, runID uuid.UUID) (*ObservationRequestRecord, error) {
	row := &ObservationRequestRecord{}
	var response []byte
	err := tx.QueryRowContext(ctx, `SELECT grant_id,grant_revision,project_uuid,environment_uuid,request_id,request_digest,actor_id,request_body,admission_deadline,response,response_status,run_id FROM environment_observation_requests WHERE org_id=$1 AND plugin_id=$2 AND (($3::uuid IS NOT NULL AND request_id=$3) OR ($4::uuid IS NOT NULL AND run_id=$4)) FOR UPDATE`, org, plugin, nullableObservationUUID(requestID), nullableObservationUUID(runID)).Scan(&row.GrantID, &row.GrantRevision, &row.ProjectUUID, &row.EnvironmentUUID, &row.RequestID, &row.RequestDigest, &row.ActorID, &row.Body, &row.Deadline, &response, &row.ResponseStatus, &row.RunID)
	row.Response = response
	return row, err
}
func nullableObservationUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func RecordObservationRequestTx(ctx context.Context, tx Tx, org, plugin string, record ObservationRequestRecord) error {
	if err := CheckObservationAdmissionOpenTx(ctx, tx, org, plugin, ObservationAdmission, record.RequestID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_observation_requests(org_id,plugin_id,request_id,grant_id,grant_revision,project_uuid,environment_uuid,request_digest,actor_id,request_body,admission_deadline) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, org, plugin, record.RequestID, record.GrantID, record.GrantRevision, record.ProjectUUID, record.EnvironmentUUID, record.RequestDigest, record.ActorID, string(record.Body), record.Deadline)
	if err != nil {
		return err
	}
	stored, err := ReadObservationRequestTx(ctx, tx, org, plugin, record.RequestID, uuid.Nil)
	if err != nil {
		return err
	}
	if stored.RequestDigest != record.RequestDigest || stored.GrantID != record.GrantID || stored.GrantRevision != record.GrantRevision || stored.ActorID != record.ActorID {
		return NewErrConflict("observation request ID already identifies a different exact request")
	}
	return nil
}
func CompleteObservationRequestTx(ctx context.Context, tx Tx, org, plugin string, request uuid.UUID, status int, body json.RawMessage, run *uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE environment_observation_requests SET response=$4,response_status=$5,run_id=$6 WHERE org_id=$1 AND plugin_id=$2 AND request_id=$3 AND response IS NULL`, org, plugin, request, string(body), status, run)
	return err
}

// ObservationRedeployment records only a fresh ordinary Deployment request's
// identity, never its ephemeral delegation proof or any executable plan.
type ObservationRedeployment struct {
	RequestID            uuid.UUID
	ObservationRequestID uuid.UUID
	RunID                uuid.UUID
	ActorID              uuid.UUID
	RequestDigest        string
	Body                 json.RawMessage
}

func ReadObservationRedeploymentTx(ctx context.Context, tx Tx, org, plugin string, id uuid.UUID) (*ObservationRedeployment, error) {
	row := &ObservationRedeployment{RequestID: id}
	err := tx.QueryRowContext(ctx, `SELECT observation_request_id,run_id,actor_id,request_digest,request_body FROM environment_observation_redeployments WHERE org_id=$1 AND plugin_id=$2 AND request_id=$3`, org, plugin, id).Scan(&row.ObservationRequestID, &row.RunID, &row.ActorID, &row.RequestDigest, &row.Body)
	return row, err
}
func RecordObservationRedeploymentTx(ctx context.Context, tx Tx, org, plugin string, record ObservationRedeployment) error {
	if err := CheckObservationAdmissionOpenTx(ctx, tx, org, plugin, ObservationRedeploymentAdmission, record.RequestID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO environment_observation_redeployments(org_id,plugin_id,request_id,observation_request_id,run_id,actor_id,request_digest,request_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, org, plugin, record.RequestID, record.ObservationRequestID, record.RunID, record.ActorID, record.RequestDigest, string(record.Body))
	if err != nil {
		return err
	}
	stored, err := ReadObservationRedeploymentTx(ctx, tx, org, plugin, record.RequestID)
	if err != nil {
		return err
	}
	if stored.RequestDigest != record.RequestDigest || stored.RunID != record.RunID || stored.ActorID != record.ActorID || stored.ObservationRequestID != record.ObservationRequestID {
		return NewErrConflict("remedial Deployment request ID already identifies another exact request or actor")
	}
	return nil
}
