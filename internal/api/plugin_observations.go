package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
)

func (s *Server) getPluginOperationGrant(ctx context.Context, org, plugin string, id uuid.UUID) (*model.ExternalOperationGrant, error) {
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, org, plugin, id)
	if err != nil {
		return nil, err
	}
	if s.PluginGateway != nil && s.PluginGateway.Registry != nil {
		if item, ok := s.PluginGateway.Registry.ConfiguredInstallation(org, plugin); ok {
			for _, op := range item.Operations {
				if op.ID == grant.SourceOperation && op.RequireOrgAdmin {
					if err := s.PluginGateway.RequireOrganizationAdmin(ctx, org, grant.ActorID.String()); err != nil {
						return nil, pluginGatewayError(err)
					}
				}
			}
		}
	}
	return grant, nil
}

func observationProof(p observation.ObservationDelegationProof) plugins.DelegationProof {
	proof := plugins.DelegationProof{OperationID: p.OperationId, IdempotencyKey: p.IdempotencyKey, Assertion: p.Assertion}
	if p.PathParameters != nil {
		proof.PathParameters = *p.PathParameters
	}
	if p.Query != nil {
		proof.Query = *p.Query
	}
	if p.Body != nil {
		proof.Body = *p.Body
	}
	return proof
}
func observationGrantAPI(grant *model.ObservationGrant) (observation.ObservationGrant, error) {
	var result observation.ObservationGrant
	encoded, err := json.Marshal(grant)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(encoded, &result)
	return result, err
}
func (s *Server) AuthorizeObservationGrant(ctx context.Context, request observation.AuthorizeObservationGrantRequestObject) (observation.AuthorizeObservationGrantResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact observation authority command is required.")
	}
	body := request.Body
	action := "observation.grant." + string(body.Action)
	safety := body.Action == observation.Revoke
	if err := pluginServiceAllowed(item, descriptor, action, true, safety); err != nil {
		return nil, err
	}
	proof := observationProof(body.Proof)
	claims, err := s.PluginGateway.VerifyServiceDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, action, proof)
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, request.OrgId, claims.ActorID); err != nil {
		return nil, pluginGatewayError(err)
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated Admin is required.")
	}
	tx, err := s.pluginEffectDatabase(request.OrgId, item, action, safety).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Membership is re-read after the effect fence, including receipt replay.
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, request.OrgId, claims.ActorID); err != nil {
		return nil, pluginGatewayError(err)
	}
	command := model.ObservationGrantCommand{OrgID: request.OrgId, PluginID: request.PluginId, GrantID: request.GrantId, ActorID: actor, SourceOperation: proof.OperationID, SourceDigest: claims.BodySHA256, RequestID: claims.RequestID, ExpectedRevision: body.ExpectedRevision, Action: string(body.Action), Reason: body.Reason, ExpiresAt: body.ExpiresAt}
	for _, target := range body.Targets {
		command.Targets = append(command.Targets, model.ExternalOperationEnvironment{ProjectID: target.ProjectId, EnvironmentID: target.EnvironmentId})
	}
	grant, err := model.ApplyObservationGrantCommandTx(ctx, tx, command, time.Now().UTC())
	if err != nil {
		return nil, pluginModelError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result, err := observationGrantAPI(grant)
	return observation.AuthorizeObservationGrant200JSONResponse(result), err
}
func (s *Server) GetObservationGrant(ctx context.Context, request observation.GetObservationGrantRequestObject) (observation.GetObservationGrantResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	action := "observation.grant.get"
	if err := pluginServiceAllowed(item, descriptor, action, false, true); err != nil {
		return nil, err
	}
	tx, err := s.pluginEffectDatabase(request.OrgId, item, action, true).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	grant, err := model.ReadObservationGrantTx(ctx, tx, request.OrgId, request.PluginId, request.GrantId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	// This is retained, installation-scoped metadata, not observation authority.
	// A lost configuration acknowledgement must remain reconcilable after its
	// author leaves the organisation. New effects recheck the controller below.
	result, err := observationGrantAPI(grant)
	if err != nil {
		return nil, err
	}
	// Retain any monotonic entitlement observation made by the read-safety
	// guard before releasing the exact installation/grant fence.
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return observation.GetObservationGrant200JSONResponse(result), nil
}

// observationAuthority reacquires current forward authority independently from
// durable routing metadata. A committed intent never authorizes a later send.
func (s *Server) observationAuthority(ctx context.Context, org, plugin string, body observation.ObservationRequest) (model.TxWithCommit, *model.ObservationGrant, error) {
	const action = "observation.try-start"
	item, descriptor, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, nil, err
	}
	if err := pluginServiceAllowed(item, descriptor, action, true, false); err != nil {
		return nil, nil, err
	}
	tx, err := s.pluginEffectDatabase(org, item, action, false).BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	deny := func(err error) (model.TxWithCommit, *model.ObservationGrant, error) {
		_ = tx.Rollback()
		return nil, nil, err
	}
	if err := model.CheckObservationAdmissionOpenTx(ctx, tx, org, plugin, model.ObservationAdmission, body.RequestId); err != nil {
		return deny(pluginModelError(err))
	}
	grant, err := model.ReadObservationGrantMetadataTx(ctx, tx, org, plugin, body.GrantId)
	if err != nil {
		return deny(pluginModelError(err))
	}
	if err := model.CheckObservationGrantTargetTx(ctx, tx, grant, body.GrantRevision, body.ProjectUuid, body.EnvironmentUuid, time.Now().UTC()); err != nil {
		return deny(pluginModelError(err))
	}
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, org, grant.ActorID.String()); err != nil {
		return deny(pluginGatewayError(err))
	}
	env, err := s.Database.GetEnvironmentByUuid(ctx, tx, org, body.EnvironmentUuid, model.GetModeDefault)
	if err != nil {
		return deny(pluginModelError(err))
	}
	if env.ProjectUuid != body.ProjectUuid || env.Id != body.EnvId || env.ProjectId != body.ProjectId {
		return deny(pluginBoundaryError(403, "plugin_service_forbidden", "Observation target identities no longer match the granted Environment."))
	}
	return tx, grant, nil
}

func (s *Server) TryStartObservation(ctx context.Context, request observation.TryStartObservationRequestObject) (observation.TryStartObservationResponseObject, error) {
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An exact observation request is required.")
	}
	body := *request.Body
	if body.RequestId == uuid.Nil || body.GrantId == uuid.Nil || body.GrantRevision < 1 || body.ProjectUuid == uuid.Nil || body.EnvironmentUuid == uuid.Nil || body.Occurrence.IsZero() || body.AdmissionDeadline.IsZero() || body.AdmissionDeadline.Before(body.Occurrence) || body.AdmissionDeadline.Sub(body.Occurrence) > 10*time.Minute || body.Occurrence.After(time.Now().UTC().Add(time.Minute)) {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "Observation requires exact identities and a bounded current admission window.")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	intentTx, grant, err := s.observationAuthority(ctx, request.OrgId, request.PluginId, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = intentTx.Rollback() }()
	record := model.ObservationRequestRecord{ActorID: grant.ActorID, GrantID: body.GrantId, GrantRevision: body.GrantRevision, ProjectUUID: body.ProjectUuid, EnvironmentUUID: body.EnvironmentUuid, RequestID: body.RequestId, RequestDigest: plugincontext.Digest(encoded), Body: encoded, Deadline: body.AdmissionDeadline}
	if err := model.RecordObservationRequestTx(ctx, intentTx, request.OrgId, request.PluginId, record); err != nil {
		return nil, pluginModelError(err)
	}
	// This commit must precede every possible remote effect. Cancellation or
	// process death after DP commits cannot erase the exact GET recovery scope.
	if err := intentTx.Commit(); err != nil {
		return nil, err
	}

	effectTx, current, err := s.observationAuthority(ctx, request.OrgId, request.PluginId, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = effectTx.Rollback() }()
	stored, err := model.ReadObservationRequestTx(ctx, effectTx, request.OrgId, request.PluginId, body.RequestId, uuid.Nil)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if stored.RequestDigest != record.RequestDigest || stored.ActorID != current.ActorID {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Observation request identity changed.")
	}
	var receipt observation.ObservationReceipt
	if len(stored.Response) > 0 {
		if err := decodeObservation(stored.Response, &receipt); err != nil {
			return nil, err
		}
	} else {
		result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: current.ActorID, Action: "observation.try-start", RequestID: body.RequestId.String(), Body: encoded})
		if err != nil {
			return nil, pluginGatewayError(err)
		}
		if result.Status != 200 {
			return nil, observationUpstreamError(result)
		}
		if err := decodeObservation(result.Body, &receipt); err != nil {
			return nil, err
		}
		if receipt.RequestId != body.RequestId {
			return nil, pluginBoundaryError(502, "plugin_response_invalid", "Observation receipt identity did not match the request.")
		}
		if err := model.CompleteObservationRequestTx(ctx, effectTx, request.OrgId, request.PluginId, body.RequestId, result.Status, result.Body, receipt.RunId); err != nil {
			return nil, err
		}
	}
	if err := effectTx.Commit(); err != nil {
		return nil, err
	}
	return observation.TryStartObservation200JSONResponse(receipt), nil
}

func decodeObservation(encoded []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return pluginBoundaryError(502, "plugin_response_invalid", "The Data Plane returned an invalid observation response.")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return pluginBoundaryError(502, "plugin_response_invalid", "The Data Plane returned trailing observation response content.")
	}
	return nil
}
func observationUpstreamError(result *plugins.InvocationResult) error {
	var errorBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(result.Body, &errorBody)
	if errorBody.Code == "" {
		errorBody.Code = "observation_unavailable"
	}
	return pluginBoundaryError(result.Status, errorBody.Code, "The Data Plane could not serve this observation.")
}
func (s *Server) observationLookup(ctx context.Context, org, plugin string, requestID, runID uuid.UUID) (*plugins.InvocationResult, error) {
	// Read only immutable routing metadata first; grant locking always precedes
	// receipt locking so lookups and submissions cannot invert lock order.
	_, _, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, err
	}
	readTx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	record, err := model.ReadObservationRequestTx(ctx, readTx, org, plugin, requestID, runID)
	_ = readTx.Rollback()
	if err != nil {
		return nil, pluginModelError(err)
	}
	action := "observation.get"
	id := runID
	if requestID != uuid.Nil {
		action = "observation.request.get"
		id = requestID
	}
	item, descriptor, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, err
	}
	if err := pluginServiceAllowed(item, descriptor, action, false, true); err != nil {
		return nil, err
	}
	// Retained safe metadata belongs to the exact immutable request, not the
	// current schedule grant. Rotation/revoke stops starts without stranding
	// admitted work. No raw deployment material crosses this read port.
	tx, err := s.pluginEffectDatabase(org, item, action, true).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: org, PluginID: plugin, ActorID: record.ActorID, Action: action, RequestID: uuid.NewString(), ObservationID: id})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if result.Status != 200 {
		return nil, observationUpstreamError(result)
	}
	if requestID == uuid.Nil {
		var run observation.ObservationRun
		if err := decodeObservation(result.Body, &run); err != nil {
			return nil, err
		}
		if run.Receipt.RequestId != record.RequestID || run.Receipt.RunId == nil || *run.Receipt.RunId != runID {
			return nil, pluginBoundaryError(502, "plugin_response_invalid", "Observation run identity changed.")
		}
	}
	if requestID != uuid.Nil {
		var receipt observation.ObservationReceipt
		if err := decodeObservation(result.Body, &receipt); err != nil {
			return nil, err
		}
		if receipt.RequestId != requestID {
			return nil, pluginBoundaryError(502, "plugin_response_invalid", "Observation receipt identity changed.")
		}
		if err := model.CompleteObservationRequestTx(ctx, tx, org, plugin, requestID, 200, result.Body, receipt.RunId); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Server) GetObservationRequest(ctx context.Context, request observation.GetObservationRequestRequestObject) (observation.GetObservationRequestResponseObject, error) {
	result, err := s.observationLookup(ctx, request.OrgId, request.PluginId, request.RequestId, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var receipt observation.ObservationReceipt
	if err := decodeObservation(result.Body, &receipt); err != nil {
		return nil, err
	}
	return observation.GetObservationRequest200JSONResponse(receipt), nil
}
func (s *Server) GetObservation(ctx context.Context, request observation.GetObservationRequestObject) (observation.GetObservationResponseObject, error) {
	result, err := s.observationLookup(ctx, request.OrgId, request.PluginId, uuid.Nil, request.RunId)
	if err != nil {
		return nil, err
	}
	var run observation.ObservationRun
	if err := decodeObservation(result.Body, &run); err != nil {
		return nil, err
	}
	return observation.GetObservation200JSONResponse(run), nil
}

var _ observation.StrictServerInterface = (*Server)(nil)

// observationRedeployAuthority is deliberately independent of unattended grants.
// Every phase verifies the fresh source proof, current Admin and ordinary user
// permission inside the installation/entitlement fence it will hold across send.
func (s *Server) observationRedeployAuthority(ctx context.Context, org, plugin string, runID uuid.UUID, body observation.ObservationRedeployRequest, proof plugins.DelegationProof) (model.TxWithCommit, uuid.UUID, *model.ObservationRequestRecord, error) {
	const action = "observation.redeploy"
	item, descriptor, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, uuid.Nil, nil, err
	}
	if err := pluginServiceAllowed(item, descriptor, action, true, false); err != nil {
		return nil, uuid.Nil, nil, err
	}
	tx, err := s.pluginEffectDatabase(org, item, action, false).BeginTx(ctx, nil)
	if err != nil {
		return nil, uuid.Nil, nil, err
	}
	deny := func(err error) (model.TxWithCommit, uuid.UUID, *model.ObservationRequestRecord, error) {
		_ = tx.Rollback()
		return nil, uuid.Nil, nil, err
	}
	if err := model.CheckObservationAdmissionOpenTx(ctx, tx, org, plugin, model.ObservationRedeploymentAdmission, body.RequestId); err != nil {
		return deny(pluginModelError(err))
	}
	// BeginTx has fenced the service action's feature. The source operation can
	// require a distinct feature, so fence it in the same transaction before
	// verifying the proof without recursively resolving Registry entitlement.
	index := slices.IndexFunc(item.Operations, func(op plugins.Operation) bool { return op.ID == proof.OperationID })
	if index < 0 {
		return deny(pluginBoundaryError(403, "plugin_delegation_forbidden", "A valid declared service delegation is required."))
	}
	operation := item.Operations[index]
	if item.Licensed() {
		if err := s.PluginEntitlements.Guard(ctx, s.Database, tx, org, item, operation.RequiredFeature, true, operation.IsSafety(proof.PathParameters)); err != nil {
			return deny(err)
		}
	}
	claims, err := s.PluginGateway.VerifyServiceDelegationWithFencedInstallation(ctx, org, item, descriptor, item.ServiceUserID, action, proof)
	if err != nil {
		return deny(pluginGatewayError(err))
	}
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, org, claims.ActorID); err != nil {
		return deny(pluginGatewayError(err))
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return deny(err)
	}
	if body.RequestId.String() != claims.RequestID {
		return deny(pluginBoundaryError(400, "plugin_request_invalid", "The Deployment request ID must equal the fresh delegated request ID."))
	}
	if err := s.pluginAuthorize(ctx, actor, []orchestratoriam.ResourcePermissionCheck{environmentCheck(body.EnvironmentUuid, "deployment_write")}); err != nil {
		return deny(err)
	}
	record, err := model.ReadObservationRequestTx(ctx, tx, org, plugin, uuid.Nil, runID)
	if err != nil {
		return deny(pluginModelError(err))
	}
	if record.ProjectUUID != body.ProjectUuid || record.EnvironmentUUID != body.EnvironmentUuid {
		return deny(pluginBoundaryError(403, "plugin_service_forbidden", "The observed run does not belong to this target."))
	}
	env, err := s.Database.GetEnvironmentByUuid(ctx, tx, org, body.EnvironmentUuid, model.GetModeDefault)
	if err != nil {
		return deny(pluginModelError(err))
	}
	if env.Id != body.EnvId || env.ProjectId != body.ProjectId {
		return deny(pluginBoundaryError(403, "plugin_service_forbidden", "Deployment slugs do not match immutable identities."))
	}
	return tx, actor, record, nil
}

// Remediation commits proof-free routing before any send, then reacquires all
// fresh user authority. The durable attempt is read-only reconciliation scope.
func (s *Server) RedeployObservationBaseline(ctx context.Context, request observation.RedeployObservationBaselineRequestObject) (observation.RedeployObservationBaselineResponseObject, error) {
	if request.Body == nil || request.Body.Proof == nil {
		return nil, pluginBoundaryError(400, "plugin_delegation_required", "A fresh user delegation is required for Deployment.")
	}
	body := *request.Body
	proof := observationProof(*body.Proof)
	body.Proof = nil
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	intentTx, actor, record, err := s.observationRedeployAuthority(ctx, request.OrgId, request.PluginId, request.RunId, body, proof)
	if err != nil {
		return nil, err
	}
	defer func() { _ = intentTx.Rollback() }()
	attempt := model.ObservationRedeployment{RequestID: body.RequestId, ObservationRequestID: record.RequestID, RunID: request.RunId, ActorID: actor, RequestDigest: plugincontext.Digest(encoded), Body: encoded}
	if err := model.RecordObservationRedeploymentTx(ctx, intentTx, request.OrgId, request.PluginId, attempt); err != nil {
		return nil, pluginModelError(err)
	}
	if err := intentTx.Commit(); err != nil {
		return nil, err
	}

	effectTx, current, observed, err := s.observationRedeployAuthority(ctx, request.OrgId, request.PluginId, request.RunId, body, proof)
	if err != nil {
		return nil, err
	}
	defer func() { _ = effectTx.Rollback() }()
	stored, err := model.ReadObservationRedeploymentTx(ctx, effectTx, request.OrgId, request.PluginId, body.RequestId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	if stored.RequestDigest != attempt.RequestDigest || stored.ActorID != current || stored.RunID != request.RunId || stored.ObservationRequestID != observed.RequestID {
		return nil, pluginBoundaryError(409, "plugin_operation_conflict", "Remedial Deployment request identity changed.")
	}
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: current, Action: "observation.redeploy", RequestID: body.RequestId.String(), ObservationID: request.RunId, Body: encoded})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if result.Status != 200 {
		return nil, observationUpstreamError(result)
	}
	var receipt observation.ObservationRedeployReceipt
	if err := decodeObservation(result.Body, &receipt); err != nil {
		return nil, err
	}
	if receipt.RequestId != body.RequestId || receipt.BaselineId != body.BaselineId {
		return nil, pluginBoundaryError(502, "plugin_response_invalid", "Deployment receipt identity changed.")
	}
	if err := effectTx.Commit(); err != nil {
		return nil, err
	}
	return observation.RedeployObservationBaseline200JSONResponse(receipt), nil
}

func (s *Server) GetObservationRedeployment(ctx context.Context, request observation.GetObservationRedeploymentRequestObject) (observation.GetObservationRedeploymentResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	action := "observation.redeploy.get"
	if err := pluginServiceAllowed(item, descriptor, action, false, true); err != nil {
		return nil, err
	}
	tx, err := s.pluginEffectDatabase(request.OrgId, item, action, true).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := model.ReadObservationRedeploymentTx(ctx, tx, request.OrgId, request.PluginId, request.RequestId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	result, err := s.PluginDataPlane.Call(ctx, plugins.DataPlaneRequest{OrgID: request.OrgId, PluginID: request.PluginId, ActorID: record.ActorID, Action: action, RequestID: uuid.NewString(), ObservationID: request.RequestId})
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if result.Status != 200 {
		return nil, observationUpstreamError(result)
	}
	var receipt observation.ObservationRedeployReceipt
	if err := decodeObservation(result.Body, &receipt); err != nil {
		return nil, err
	}
	if receipt.RequestId != record.RequestID {
		return nil, pluginBoundaryError(502, "plugin_response_invalid", "Remedial Deployment receipt identity changed.")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return observation.GetObservationRedeployment200JSONResponse(receipt), nil
}
