package api

import (
	"context"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	observation "github.com/stellwerk-labs/platform-orchestrator-cp/internal/observationapi"
)

func (s *Server) QueryObservationTargets(ctx context.Context, request observation.QueryObservationTargetsRequestObject) (observation.QueryObservationTargetsResponseObject, error) {
	item, descriptor, err := s.pluginInstallationCaller(ctx, request.OrgId, request.PluginId, false)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, pluginBoundaryError(400, "plugin_request_invalid", "A fresh delegated target query is required.")
	}
	body := request.Body
	action := "observation.targets.query"
	if err := pluginServiceAllowed(item, descriptor, action, false, false); err != nil {
		return nil, err
	}
	claims, err := s.PluginGateway.VerifyServiceReadDelegation(ctx, request.OrgId, request.PluginId, item.ServiceUserID, action, observationProof(body.Proof))
	if err != nil {
		return nil, pluginGatewayError(err)
	}
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, request.OrgId, claims.ActorID); err != nil {
		return nil, pluginGatewayError(err)
	}
	// A new inventory query is a current delegated effect. It cannot use the
	// retained-result read policy to enumerate targets after disable/revocation.
	tx, err := s.pluginEffectDatabase(request.OrgId, item, action, false).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.PluginGateway.RequireOrganizationAdmin(ctx, request.OrgId, claims.ActorID); err != nil {
		return nil, pluginGatewayError(err)
	}
	selector := model.ObservationTargetSelector{ProjectIDs: body.ProjectIds, EnvironmentTypes: body.EnvironmentTypes, Production: body.Production, EnvironmentIDs: body.EnvironmentIds}
	if body.ExcludeEnvironmentIds != nil {
		selector.ExcludeEnvironmentIDs = *body.ExcludeEnvironmentIds
	}
	limit := 100
	if body.Limit != nil {
		limit = *body.Limit
	}
	page := ""
	if body.Page != nil {
		page = *body.Page
	}
	items, next, err := model.QueryObservationTargetsTx(ctx, tx, request.OrgId, request.PluginId, selector, page, limit)
	if err != nil {
		return nil, pluginModelError(err)
	}
	response := observation.QueryObservationTargets200JSONResponse{Items: []observation.ObservationTargetMetadata{}}
	for _, item := range items {
		response.Items = append(response.Items, observation.ObservationTargetMetadata{ProjectUuid: item.ProjectUUID, EnvironmentUuid: item.EnvironmentUUID, ProjectId: item.ProjectID, EnvironmentId: item.EnvironmentID, EnvironmentType: item.EnvironmentType, Production: item.Production})
	}
	if next != "" {
		response.NextPageToken = &next
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}
