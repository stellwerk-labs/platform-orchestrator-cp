package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
)

const maxDeploymentOutcomeReasonLength = 1000

// AcknowledgeDeploymentOutcome forwards an operator's acknowledgement of an
// unknown Runner outcome to the Data Plane, which keeps the authoritative,
// attributed record. Core adds the Environment scope and authorizes the actor
// itself; the Data Plane authorizes the same actor again.
func (s *Server) AcknowledgeDeploymentOutcome(ctx context.Context, request AcknowledgeDeploymentOutcomeRequestObject) (AcknowledgeDeploymentOutcomeResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	// The actor is the audit record, so neither the system user nor the
	// deprecated organization token, which bypass IAM in Core, may act.
	if actor == userid.InternalSystemUuid || organizationTokenUsed(ctx) {
		return AcknowledgeDeploymentOutcome403JSONResponse{N403ForbiddenJSONResponse{Error: "HTTP-403", Message: "acknowledging a Deployment outcome requires an attributable user"}}, nil
	}
	if err := s.checkEnvAuthorization(ctx, actor, request.OrgId, request.ProjectId, request.EnvId, PermissionDeploymentOutcomeAcknowledge); err != nil {
		return nil, err
	}
	reason := ""
	if request.Body != nil {
		reason = strings.TrimSpace(request.Body.Reason)
	}
	if reason == "" || utf8.RuneCountInString(reason) > maxDeploymentOutcomeReasonLength {
		return AcknowledgeDeploymentOutcome400JSONResponse{Generate400Response("a reason of 1 to 1000 characters is required")}, nil
	}
	return s.forwardDeploymentOutcomeAcknowledgement(ctx, actor, request.OrgId, request.ProjectId, request.EnvId, request.DeploymentId, reason)
}

// forwardDeploymentOutcomeAcknowledgement confines the Deployment to the
// addressed Environment, then lets the Data Plane decide and record. Its 400,
// 403, 404 and 409 diagnostics pass through unchanged.
func (s *Server) forwardDeploymentOutcomeAcknowledgement(ctx context.Context, actor uuid.UUID, orgID, projectID, envID string, deploymentID uuid.UUID, reason string) (AcknowledgeDeploymentOutcomeResponseObject, error) {
	deployment, err := s.DpClient.GetDeploymentWithResponse(ctx, orgID, deploymentID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read the Deployment from the data plane")
	}
	notInEnvironment := AcknowledgeDeploymentOutcome404JSONResponse{N404NotFoundJSONResponse{Error: "HTTP-404", Message: "the Deployment does not exist in this Environment"}}
	switch {
	case deployment.StatusCode() == http.StatusNotFound:
		return notInEnvironment, nil
	case deployment.JSON200 == nil:
		return nil, errors.Errorf("unexpected data plane response when reading the Deployment: %s", deployment.Status())
	case deployment.JSON200.ProjectId != projectID || deployment.JSON200.EnvId != envID:
		return notInEnvironment, nil
	}

	response, err := s.DpClient.InternalAcknowledgeDeploymentOutcomeWithResponse(ctx, orgID, deploymentID,
		orchestratordp.DeploymentOutcomeAcknowledgementBody{Reason: reason}, actingUser(actor))
	if err != nil {
		return nil, errors.Wrap(err, "failed to acknowledge the Deployment outcome in the data plane")
	}
	switch {
	case response.JSON201 != nil, response.JSON200 != nil:
		ack := response.JSON201
		if ack == nil {
			ack = response.JSON200
		}
		if ack.ActorId != actor || ack.DeploymentId != deploymentID {
			return nil, errors.Errorf("the data plane returned an acknowledgement for another actor or Deployment")
		}
		result := deploymentOutcomeAcknowledgement(*ack)
		if response.JSON200 != nil {
			return AcknowledgeDeploymentOutcome200JSONResponse(result), nil
		}
		hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).Info("acknowledged unreconciled Deployment outcome",
			zap.String("org_id", orgID), zap.String("project_id", projectID), zap.String("env_id", envID),
			zap.String("deployment_id", deploymentID.String()), zap.String("plugin_id", ack.PluginId),
			zap.String("actor_id", actor.String()))
		return AcknowledgeDeploymentOutcome201JSONResponse(result), nil
	case response.JSON400 != nil:
		return AcknowledgeDeploymentOutcome400JSONResponse{N400BadRequestJSONResponse(coreError(*response.JSON400))}, nil
	case response.JSON404 != nil:
		return AcknowledgeDeploymentOutcome404JSONResponse{N404NotFoundJSONResponse(coreError(*response.JSON404))}, nil
	case response.JSON409 != nil:
		return AcknowledgeDeploymentOutcome409JSONResponse{N409ConflictJSONResponse(coreError(*response.JSON409))}, nil
	case response.StatusCode() == http.StatusForbidden:
		return AcknowledgeDeploymentOutcome403JSONResponse{N403ForbiddenJSONResponse(dataPlaneDenial(response.Body))}, nil
	}
	return nil, errors.Errorf("unexpected data plane response when acknowledging the Deployment outcome: %s", response.Status())
}

// actingUser replaces the client's system identity with the authenticated
// actor, so the Data Plane authorizes and records the person who acted.
func actingUser(actor uuid.UUID) orchestratordp.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("From", actor.String())
		return nil
	}
}

// organizationTokenUsed reports the deprecated organization token. It
// identifies no person and therefore cannot be the actor of an audit record.
func organizationTokenUsed(ctx context.Context) bool {
	c := middleware.GetEchoCtx(ctx)
	return c != nil && c.Request().Header.Get("Po-Org-Id") != ""
}

func coreError(e orchestratordp.Error) Error {
	return Error{Error: e.Error, Message: e.Message, Details: e.Details}
}

// dataPlaneDenial keeps the Data Plane's 403 diagnostic when it has one.
func dataPlaneDenial(body []byte) Error {
	var denial Error
	if json.Unmarshal(body, &denial) != nil || denial.Message == "" {
		denial.Message = "the data plane denied acknowledging this Deployment outcome"
	}
	if denial.Error == "" {
		denial.Error = "HTTP-403"
	}
	return denial
}

func deploymentOutcomeAcknowledgement(ack orchestratordp.DeploymentOutcomeAcknowledgement) DeploymentOutcomeAcknowledgement {
	return DeploymentOutcomeAcknowledgement{
		AcknowledgedAt: ack.AcknowledgedAt, ActorId: ack.ActorId, DeploymentId: ack.DeploymentId,
		EnvironmentId: ack.EnvironmentId, OperationRef: ack.OperationRef, PluginId: ack.PluginId,
		Reason: ack.Reason, Replayed: ack.Replayed,
	}
}
