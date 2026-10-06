package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

type pluginResourceCursor struct {
	Org       string    `json:"org"`
	Plugin    string    `json:"plugin"`
	Operation uuid.UUID `json:"operation"`
	Sequence  int64     `json:"sequence"`
}

// Event reconciliation does not expand an operation's frozen scope and remains
// useful after release. Current IAM is still required for the original actor.
func (s *Server) pluginEventGrant(ctx context.Context, org, plugin string, operation uuid.UUID, action string) (*model.ExternalOperationGrant, plugins.Installation, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, org, plugin, false)
	if err != nil {
		return nil, item, err
	}
	grant, err := model.GetExternalOperationGrant(ctx, s.Database, org, plugin, operation)
	if err != nil {
		return nil, item, pluginModelError(err)
	}
	policy, ok := item.ServiceGrant(action)
	if !ok || !slices.Contains(grant.Intent.Actions, action) {
		return nil, item, pluginBoundaryError(403, "plugin_service_forbidden", "The frozen operation did not delegate this event action.")
	}
	if err := pluginFeatureAllowed(descriptor, policy.RequiredFeature, false, false); err != nil {
		return nil, item, err
	}
	checks := []orchestratoriam.ResourcePermissionCheck{}
	if policy.Scope == pluginOrganizationScope || len(grant.Intent.Environments) == 0 {
		checks = append(checks, orgCheck(org, policy.Permission))
	} else {
		for _, environment := range grant.Intent.Environments {
			checks = append(checks, environmentCheck(environment.EnvironmentID, policy.Permission))
		}
	}
	if err := s.pluginAuthorize(ctx, grant.ActorID, checks); err != nil {
		return nil, item, err
	}
	return grant, item, nil
}

func (s *Server) ReadPluginResourceEvents(ctx context.Context, request ReadPluginResourceEventsRequestObject) (ReadPluginResourceEventsResponseObject, error) {
	grant, item, err := s.pluginEventGrant(ctx, request.OrgId, request.PluginId, request.OperationId, plugins.ResourceEventsReadAction)
	if err != nil {
		return nil, err
	}
	cursor := pluginResourceCursor{Org: request.OrgId, Plugin: request.PluginId, Operation: request.OperationId}
	var after *int64
	if request.Params.After != nil && *request.Params.After != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(*request.Params.After)
		var decoded pluginResourceCursor
		if err != nil || json.Unmarshal(encoded, &decoded) != nil || decoded.Org != cursor.Org || decoded.Plugin != cursor.Plugin || decoded.Operation != cursor.Operation || decoded.Sequence < 0 {
			return nil, pluginBoundaryError(400, "plugin_cursor_invalid", "Use a cursor returned for this exact operation.")
		}
		presented := decoded.Sequence
		after = &presented
	}
	events, next, err := model.ReadExtensionResourceEvents(ctx, s.Database, *grant, after, ref.DerefOr(request.Params.Limit, 100))
	if err != nil {
		return nil, pluginResourceEventError(err)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	response := ReadPluginResourceEvents200JSONResponse{OrganizationTimezone: item.OrganizationTimezones[request.OrgId]}
	if err := json.Unmarshal(encoded, &response.Events); err != nil {
		return nil, err
	}
	cursor.Sequence = next
	encoded, err = json.Marshal(cursor)
	if err != nil {
		return nil, err
	}
	response.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	return response, nil
}

// pluginResourceEventError keeps an expired cursor distinguishable from an
// ordinary conflict, so a consumer rebuilds its state instead of retrying.
func pluginResourceEventError(err error) error {
	if errors.Is(err, model.ErrExtensionResourceCursorExpired) {
		return pluginBoundaryError(409, "plugin_resource_cursor_expired", "Retention removed resource events after this cursor. Rebuild state from current Core resources, then read again without a cursor.")
	}
	return pluginModelError(err)
}

func (s *Server) PublishPluginEvent(ctx context.Context, request PublishPluginEventRequestObject) (PublishPluginEventResponseObject, error) {
	grant, item, err := s.pluginEventGrant(ctx, request.OrgId, request.PluginId, request.OperationId, "events.publish")
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "A complete event publication is required.")
	}
	body := request.Body
	if err := model.PublishExtensionEvent(ctx, s.pluginEffectDatabase(request.OrgId, item, "events.publish", true), *grant, model.ExtensionPublishedEvent{EventID: body.EventId, GrantFence: body.GrantFence, Type: body.Type, OccurredAt: body.OccurredAt, Data: body.Data}); err != nil {
		if errors.Is(err, model.ErrExtensionEventFence) {
			return nil, pluginBoundaryError(409, "plugin_event_stale_fence", "No receipt exists for this event ID and the grant fence changed. Read the current grant before retrying.")
		}
		if errors.Is(err, model.ErrExtensionEventReused) {
			return nil, pluginBoundaryError(409, "plugin_event_id_conflict", "This event ID already identifies a different publication. Replay the original request exactly.")
		}
		return nil, pluginModelError(err)
	}
	return PublishPluginEvent204Response{}, nil
}
