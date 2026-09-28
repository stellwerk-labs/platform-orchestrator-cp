package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func (s *Server) GetPluginOperationGrant(ctx context.Context, request GetPluginOperationGrantRequestObject) (GetPluginOperationGrantResponseObject, error) {
	if _, _, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false); err != nil {
		return nil, err
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId)
	if err != nil {
		return nil, pluginModelError(err)
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	var response GetPluginOperationGrant200JSONResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *Server) AuthorizePluginOperation(ctx context.Context, request AuthorizePluginOperationRequestObject) (AuthorizePluginOperationResponseObject, error) {
	item, _, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "An operation authorization is required.")
	}
	body := request.Body
	proof := plugins.DelegationProof{OperationID: body.Proof.OperationId, PathParameters: ref.DerefOr(body.Proof.PathParameters, map[string]string{}), Query: ref.DerefOr(body.Proof.Query, map[string][]string{}), Body: ref.DerefOr(body.Proof.Body, []byte{}), IdempotencyKey: body.Proof.IdempotencyKey, Assertion: body.Proof.Assertion}
	action := "operation." + string(body.Action)
	claims, err := s.PluginGateway.VerifyServiceDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, action, proof)
	if err != nil {
		var denied *plugins.GatewayError
		if errors.As(err, &denied) {
			return nil, pluginBoundaryError(denied.Status, denied.Code, denied.Message)
		}
		return nil, err
	}
	actor, err := uuid.Parse(claims.ActorID)
	if err != nil {
		return nil, pluginBoundaryError(403, "plugin_actor_forbidden", "An authenticated delegated actor is required.")
	}
	command := model.ExternalGrantCommand{OrgID: request.OrgId, PluginID: request.PluginId, OperationID: request.OperationId, ActorID: actor, SourceOperation: proof.OperationID, SourceDigest: claims.BodySHA256, RequestID: claims.RequestID, ExpectedRevision: body.ExpectedRevision, Action: string(body.Action), Reason: body.Reason}
	var intent model.ExternalOperationIntent
	if body.Intent != nil {
		encoded, err := json.Marshal(body.Intent)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &intent); err != nil {
			return nil, err
		}
		command.Intent = &intent
	} else {
		grant, err := model.GetExternalOperationGrant(ctx, s.Database, request.OrgId, request.PluginId, request.OperationId)
		if err != nil {
			return nil, pluginModelError(err)
		}
		intent = grant.Intent
	}
	// Only service actions explicitly delegated by this source API operation may
	// become durable. Permission names come from operator policy, never the body.
	source := item.Operations[slices.IndexFunc(item.Operations, func(op plugins.Operation) bool { return op.ID == proof.OperationID })]
	checks := []orchestratoriam.ResourcePermissionCheck{}
	for _, requested := range append([]string{action}, intent.Actions...) {
		policy, exists := item.ServiceGrant(requested)
		if !exists || body.Action == "prepare" && !slices.Contains(source.ServiceActions, requested) {
			return nil, pluginBoundaryError(403, "plugin_service_forbidden", "The source operation cannot authorize the requested service action.")
		}
		if requested != action {
			continue
		}
		if policy.Scope == pluginOrganizationScope {
			checks = append(checks, orgCheck(request.OrgId, policy.Permission))
		} else {
			for _, environment := range intent.Environments {
				checks = append(checks, environmentCheck(environment.EnvironmentID, policy.Permission))
			}
		}
	}
	if err := s.pluginAuthorize(ctx, actor, checks); err != nil {
		return nil, err
	}
	grant, err := model.ApplyExternalGrantCommand(ctx, s.Database, command)
	if err != nil {
		return nil, pluginModelError(err)
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	var response AuthorizePluginOperation200JSONResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, err
	}
	return response, nil
}
