package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

func moduleDraftTestSnapshotToAPI(value *model.ModuleDraftTestSnapshot) (ModuleDraftTestSnapshot, error) {
	var definition ModuleVersionPublishBody
	if err := json.Unmarshal(value.Content, &definition); err != nil {
		return ModuleDraftTestSnapshot{}, err
	}
	return ModuleDraftTestSnapshot{Id: value.ID, Provenance: DraftTest, OrgId: value.OrgID, ModuleUuid: value.ModuleUUID, ModuleSlug: value.ModuleSlug,
		DraftId: value.DraftID, DraftRevision: value.DraftRevision, ResourceTypeVersionUuid: value.ResourceTypeVersionUUID, ResourceTypeVersion: value.ResourceTypeVersion,
		ProjectUuid: value.ProjectUUID, EnvironmentUuid: value.EnvironmentUUID, Definition: definition, CapturedAt: value.CapturedAt, CapturedBy: value.CapturedBy, Reason: value.Reason}, nil
}

func (s *Server) CaptureModuleDraftTestSnapshot(ctx context.Context, request CaptureModuleDraftTestSnapshotRequestObject) (CaptureModuleDraftTestSnapshotResponseObject, error) {
	actor, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionPublish)
	if err != nil {
		return nil, err
	}
	// Recheck scoped deployment authority before receipt replay as well as capture.
	if err := s.checkEnvAuthorization(ctx, actor, request.OrgId, request.ProjectId, request.EnvId, "deployment_write"); err != nil {
		return nil, err
	}
	environment, err := s.Database.GetEnvironment(ctx, nil, request.OrgId, request.ProjectId, request.EnvId, model.GetModeDefault)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	response, err := runModuleDraftCommand(ctx, s, request.OrgId, "draft-test-capture:"+request.DraftId.String()+":"+environment.Uuid.String(), request.Params.IdempotencyKey, request,
		func(tx model.Tx, actor uuid.UUID) (ModuleDraftTestSnapshot, error) {
			draft, err := s.Database.GetModuleDraft(ctx, tx, request.OrgId, request.ModuleId, request.DraftId, model.GetModeDefault)
			if err != nil {
				return ModuleDraftTestSnapshot{}, err
			}
			if draft.ResourceVersion != request.Body.ExpectedResourceVersion {
				return ModuleDraftTestSnapshot{}, model.NewErrConflict("Draft changed since review")
			}
			var body ModuleVersionPublishBody
			if err := json.Unmarshal(draft.Content, &body); err != nil {
				return ModuleDraftTestSnapshot{}, err
			}
			if err := validatePublicationBody(body); err != nil {
				return ModuleDraftTestSnapshot{}, model.NewErrBadRequest(err.Error())
			}
			module, err := s.Database.GetModuleCatalogue(ctx, tx, request.OrgId, request.ModuleId, model.GetModeDefault)
			if err != nil {
				return ModuleDraftTestSnapshot{}, err
			}
			if missing, err := s.checkTypesExist(ctx, tx, request.OrgId, publicationReferencedTypes(module, body)); err != nil {
				return ModuleDraftTestSnapshot{}, err
			} else if len(missing) > 0 {
				return ModuleDraftTestSnapshot{}, model.NewErrBadRequest("Draft test references missing Resource Types")
			}
			captured, err := s.Database.CaptureModuleDraftTestSnapshot(ctx, tx, request.OrgId, request.ModuleId, model.ModuleDraftTestCapture{
				DraftID: request.DraftId, ExpectedDraftRevision: request.Body.ExpectedResourceVersion, EnvironmentUUID: environment.Uuid, Actor: actor, Reason: request.Body.Reason})
			if err != nil {
				return ModuleDraftTestSnapshot{}, err
			}
			return moduleDraftTestSnapshotToAPI(captured)
		})
	return CaptureModuleDraftTestSnapshot201JSONResponse(response), err
}

func (s *Server) GetModuleDraftTestSnapshot(ctx context.Context, request GetModuleDraftTestSnapshotRequestObject) (GetModuleDraftTestSnapshotResponseObject, error) {
	actor, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionRead)
	if err != nil {
		return nil, err
	}
	if err := s.checkEnvAuthorization(ctx, actor, request.OrgId, request.ProjectId, request.EnvId, "deployment_read"); err != nil {
		return nil, err
	}
	environment, err := s.Database.GetEnvironment(ctx, nil, request.OrgId, request.ProjectId, request.EnvId, model.GetModeDefault)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	snapshot, err := s.Database.GetModuleDraftTestSnapshot(ctx, nil, request.OrgId, request.ModuleId, environment.Uuid, request.SnapshotId)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	response, err := moduleDraftTestSnapshotToAPI(snapshot)
	return GetModuleDraftTestSnapshot200JSONResponse(response), err
}
