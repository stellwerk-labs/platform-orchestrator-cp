package api

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

func (s *Server) observationRecoveryTransaction(ctx context.Context, org, plugin, kind string, id uuid.UUID) (model.TxWithCommit, uuid.UUID, error) {
	action := "observation.request.resolve"
	if kind == model.ObservationRedeploymentAdmission {
		action = "observation.redeploy.resolve"
	}
	item, descriptor, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if err := pluginServiceAllowed(item, descriptor, action, true, true); err != nil {
		return nil, uuid.Nil, err
	}
	tx, err := s.pluginEffectDatabase(org, item, action, true).BeginTx(ctx, nil)
	if err != nil {
		return nil, uuid.Nil, err
	}
	if err := model.LockObservationAdmissionTx(ctx, tx, org, plugin, kind, id); err != nil {
		_ = tx.Rollback()
		return nil, uuid.Nil, pluginModelError(err)
	}
	actor, err := uuid.Parse(item.ServiceUserID)
	if err != nil {
		_ = tx.Rollback()
		return nil, uuid.Nil, err
	}
	return tx, actor, nil
}

func observationRecoveryBody(kind string, raw []byte) ([]byte, error) {
	if kind == model.ObservationAdmission {
		var body observation.ObservationRequest
		if err := decodeObservation(raw, &body); err != nil {
			return nil, err
		}
		return json.Marshal(body)
	}
	var body observation.ObservationRedeployRequest
	if err := decodeObservation(raw, &body); err != nil {
		return nil, err
	}
	if body.Proof != nil {
		return nil, pluginBoundaryError(502, "plugin_response_invalid", "A retained Deployment request contains unexpected proof material.")
	}
	return json.Marshal(body)
}

// closeObservationAdmission commits the no-forward decision before contacting DP.
// A missing journal can be settled locally only after the caller's exact
// historical scope is verified. Such a closure has no invented human actor.
func (s *Server) closeObservationAdmission(ctx context.Context, org, plugin, kind string, id uuid.UUID, body []byte, run *uuid.UUID, validateMissing func(model.Tx) error) ([]byte, error) {
	digest := plugincontext.Digest(body)
	tx, service, err := s.observationRecoveryTransaction(ctx, org, plugin, kind, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	closure, err := model.ReadObservationClosureTx(ctx, tx, org, plugin, kind, id)
	if errors.Is(err, sql.ErrNoRows) {
		closure = &model.ObservationClosure{Kind: kind, RequestID: id, RequestDigest: digest, Body: body, RunID: run, ServiceActorID: service}
		if kind == model.ObservationAdmission {
			record, e := model.ReadObservationRequestTx(ctx, tx, org, plugin, id, uuid.Nil)
			if e == nil {
				closure.RequestDigest = record.RequestDigest
				closure.Body = record.Body
				closure.OriginalActorID = &record.ActorID
			} else if !errors.Is(e, sql.ErrNoRows) {
				return nil, pluginModelError(e)
			}
		} else {
			record, e := model.ReadObservationRedeploymentTx(ctx, tx, org, plugin, id)
			if e == nil {
				closure.RequestDigest = record.RequestDigest
				closure.Body = record.Body
				closure.OriginalActorID = &record.ActorID
				closure.RunID = &record.RunID
			} else if !errors.Is(e, sql.ErrNoRows) {
				return nil, pluginModelError(e)
			}
		}
		if closure.RequestDigest != digest || !sameObservationRun(run, closure.RunID) {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Resolution must retain the exact original request.")
		}
		if closure.OriginalActorID == nil {
			if err := validateMissing(tx); err != nil {
				return nil, err
			}
		}
		closure.Body, err = observationRecoveryBody(kind, closure.Body)
		if err != nil {
			return nil, err
		}
		if plugincontext.Digest(closure.Body) != digest {
			return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The retained request identity cannot be reconstructed exactly.")
		}
		closure, err = model.RecordObservationClosureTx(ctx, tx, org, plugin, *closure)
		if err != nil {
			return nil, pluginModelError(err)
		}
		if closure.OriginalActorID == nil {
			resolved, e := observationNotStarted(kind, id)
			if e != nil {
				return nil, e
			}
			if e = model.CompleteObservationClosureTx(ctx, tx, org, plugin, kind, id, "not_started", resolved); e != nil {
				return nil, e
			}
		}
	} else if err != nil {
		return nil, pluginModelError(err)
	}
	if closure.RequestDigest != digest || !sameObservationRun(run, closure.RunID) {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Resolution must retain the exact original request.")
	}
	// Never combine this durable close with a later remote-effect transaction.
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	effect, _, err := s.observationRecoveryTransaction(ctx, org, plugin, kind, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = effect.Rollback() }()
	closed, err := model.ReadObservationClosureTx(ctx, effect, org, plugin, kind, id)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if len(closed.Response) > 0 {
		if err := effect.Commit(); err != nil {
			return nil, err
		}
		return closed.Response, nil
	}
	if closed.OriginalActorID == nil {
		return nil, pluginBoundaryError(503, "observation_unavailable", "The exact admission closure is not reconciled.")
	}
	encoded, err := observationRecoveryBody(kind, closed.Body)
	if err != nil {
		return nil, err
	}
	if plugincontext.Digest(encoded) != closed.RequestDigest {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "The retained request identity changed.")
	}
	action := "observation.request.resolve"
	if kind == model.ObservationRedeploymentAdmission {
		action = "observation.redeploy.resolve"
		var request observation.ObservationRedeployRequest
		if err := json.Unmarshal(encoded, &request); err != nil {
			return nil, err
		}
		encoded, err = json.Marshal(observation.ObservationRedeployResolveRequest{RunId: *closed.RunID, Request: request})
		if err != nil {
			return nil, err
		}
	}
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: org, PluginID: plugin, ActorID: *closed.OriginalActorID, Action: action, RequestID: id.String(), ObservationID: id, Body: encoded})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if result.Status != 200 {
		return nil, observationUpstreamError(result)
	}
	state, err := s.recordObservationResolution(ctx, effect, org, plugin, kind, id, result.Body)
	if err != nil {
		return nil, err
	}
	if err := model.CompleteObservationClosureTx(ctx, effect, org, plugin, kind, id, state, result.Body); err != nil {
		return nil, err
	}
	if err := effect.Commit(); err != nil {
		return nil, err
	}
	return result.Body, nil
}
func sameObservationRun(a, b *uuid.UUID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func observationNotStarted(kind string, id uuid.UUID) ([]byte, error) {
	if kind == model.ObservationAdmission {
		return json.Marshal(observation.ObservationResolution{RequestId: id, Disposition: observation.ObservationResolutionDispositionNotStarted})
	}
	return json.Marshal(observation.ObservationRedeployResolution{RequestId: id, Disposition: observation.ObservationRedeployResolutionDispositionNotStarted})
}
func (s *Server) recordObservationResolution(ctx context.Context, tx model.Tx, org, plugin, kind string, id uuid.UUID, raw []byte) (string, error) {
	invalid := pluginBoundaryError(502, "plugin_response_invalid", "The Data Plane returned an invalid exact admission resolution.")
	if kind == model.ObservationAdmission {
		var result observation.ObservationResolution
		if err := decodeObservation(raw, &result); err != nil {
			return "", err
		}
		if result.RequestId != id || !result.Disposition.Valid() || (result.Disposition == observation.ObservationResolutionDispositionAccepted) != (result.Receipt != nil) {
			return "", invalid
		}
		if result.Receipt != nil {
			r := result.Receipt
			if r.RequestId != id || r.Disposition != observation.ObservationReceiptDispositionAccepted || r.RunId == nil || *r.RunId == uuid.Nil || r.BaselineId == nil || *r.BaselineId == uuid.Nil || r.BaselineFingerprint == nil || !validObservationFingerprint(*r.BaselineFingerprint) || r.Reason != observation.ObservationReceiptReasonAccepted || r.RecordedAt.IsZero() {
				return "", invalid
			}
			stored, err := model.ReadObservationRequestTx(ctx, tx, org, plugin, id, uuid.Nil)
			if err != nil {
				return "", pluginModelError(err)
			}
			if stored.RunID != nil && *stored.RunID != *r.RunId {
				return "", invalid
			}
			if len(stored.Response) > 0 {
				var previous observation.ObservationReceipt
				if json.Unmarshal(stored.Response, &previous) != nil || previous.Disposition != r.Disposition || previous.RunId == nil || *previous.RunId != *r.RunId || previous.BaselineId == nil || *previous.BaselineId != *r.BaselineId || previous.BaselineFingerprint == nil || *previous.BaselineFingerprint != *r.BaselineFingerprint {
					return "", invalid
				}
			}
			receipt, err := json.Marshal(r)
			if err != nil {
				return "", err
			}
			if err := model.CompleteObservationRequestTx(ctx, tx, org, plugin, id, 200, receipt, r.RunId); err != nil {
				return "", err
			}
		}
		return string(result.Disposition), nil
	}
	var result observation.ObservationRedeployResolution
	if err := decodeObservation(raw, &result); err != nil {
		return "", err
	}
	if result.RequestId != id || !result.Disposition.Valid() || (result.Disposition == observation.ObservationRedeployResolutionDispositionAccepted) != (result.Receipt != nil) {
		return "", invalid
	}
	if result.Receipt != nil && (result.Receipt.RequestId != id || result.Receipt.DeploymentId == uuid.Nil || result.Receipt.BaselineId == uuid.Nil || !result.Receipt.Status.Valid()) {
		return "", invalid
	}
	if result.Receipt != nil {
		original, err := model.ReadObservationRedeploymentTx(ctx, tx, org, plugin, id)
		if err != nil {
			return "", pluginModelError(err)
		}
		var requested observation.ObservationRedeployRequest
		if json.Unmarshal(original.Body, &requested) != nil || requested.BaselineId != result.Receipt.BaselineId {
			return "", invalid
		}
	}
	return string(result.Disposition), nil
}

func (s *Server) ResolveObservationRequest(ctx context.Context, request observation.ResolveObservationRequestRequestObject) (observation.ResolveObservationRequestResponseObject, error) {
	if request.Body == nil || request.RequestId == uuid.Nil || request.Body.RequestId != request.RequestId || request.Body.GrantId == uuid.Nil || request.Body.GrantRevision < 1 || request.Body.ProjectUuid == uuid.Nil || request.Body.EnvironmentUuid == uuid.Nil || request.Body.Occurrence.IsZero() || request.Body.AdmissionDeadline.IsZero() || request.Body.AdmissionDeadline.Before(request.Body.Occurrence) || request.Body.AdmissionDeadline.Sub(request.Body.Occurrence) > 10*time.Minute {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Resolution requires an exact expired observation admission key.")
	}
	if request.Body.AdmissionDeadline.After(time.Now().UTC()) {
		policy, err := model.ReadInstallationMaintenance(ctx, s.Database)
		if err != nil {
			return nil, err
		}
		if s.MaintenanceDataPlaneURL == "" || !policy.Enabled || policy.Revision < 1 {
			return nil, pluginBoundaryError(400, "plugin_request_invalid", "Early closure requires an applied maintenance barrier.")
		}
	}
	body, err := json.Marshal(request.Body)
	if err != nil {
		return nil, err
	}
	missing := func(tx model.Tx) error {
		if err := model.CheckIssuedObservationTargetTx(ctx, tx, request.OrgId, request.PluginId, request.Body.GrantId, request.Body.GrantRevision, request.Body.ProjectUuid, request.Body.EnvironmentUuid); err != nil {
			return pluginBoundaryError(403, "plugin_service_forbidden", "An exact historically issued observation target is required.")
		}
		return nil
	}
	result, err := s.closeObservationAdmission(ctx, request.OrgId, request.PluginId, model.ObservationAdmission, request.RequestId, body, nil, missing)
	if err != nil {
		return nil, err
	}
	var resolution observation.ObservationResolution
	if err := decodeObservation(result, &resolution); err != nil {
		return nil, err
	}
	return observation.ResolveObservationRequest200JSONResponse(resolution), nil
}
func (s *Server) ResolveObservationRedeployment(ctx context.Context, request observation.ResolveObservationRedeploymentRequestObject) (observation.ResolveObservationRedeploymentResponseObject, error) {
	if request.Body == nil || request.RequestId == uuid.Nil || request.Body.Request.RequestId != request.RequestId || request.Body.RunId == uuid.Nil || request.Body.Request.Proof != nil || request.Body.Request.ProjectUuid == uuid.Nil || request.Body.Request.EnvironmentUuid == uuid.Nil || request.Body.Request.BaselineId == uuid.Nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Resolution requires the exact proof-free remedial admission key and observed run.")
	}
	body, err := json.Marshal(request.Body.Request)
	if err != nil {
		return nil, err
	}
	missing := func(tx model.Tx) error {
		record, e := model.ReadObservationRequestTx(ctx, tx, request.OrgId, request.PluginId, uuid.Nil, request.Body.RunId)
		if e != nil {
			return pluginModelError(e)
		}
		var observed observation.ObservationRequest
		var receipt observation.ObservationReceipt
		if json.Unmarshal(record.Body, &observed) != nil || json.Unmarshal(record.Response, &receipt) != nil || receipt.Disposition != observation.ObservationReceiptDispositionAccepted || receipt.RunId == nil || *receipt.RunId != request.Body.RunId || receipt.BaselineId == nil || *receipt.BaselineId != request.Body.Request.BaselineId || receipt.BaselineFingerprint == nil || *receipt.BaselineFingerprint != request.Body.Request.BaselineFingerprint || record.ProjectUUID != request.Body.Request.ProjectUuid || record.EnvironmentUUID != request.Body.Request.EnvironmentUuid || observed.ProjectId != request.Body.Request.ProjectId || observed.EnvId != request.Body.Request.EnvId {
			return pluginBoundaryError(403, "plugin_service_forbidden", "The remedial key is outside the retained observed baseline scope.")
		}
		return nil
	}
	result, err := s.closeObservationAdmission(ctx, request.OrgId, request.PluginId, model.ObservationRedeploymentAdmission, request.RequestId, body, &request.Body.RunId, missing)
	if err != nil {
		return nil, err
	}
	var resolution observation.ObservationRedeployResolution
	if err := decodeObservation(result, &resolution); err != nil {
		return nil, err
	}
	return observation.ResolveObservationRedeployment200JSONResponse(resolution), nil
}

func validObservationFingerprint(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}
