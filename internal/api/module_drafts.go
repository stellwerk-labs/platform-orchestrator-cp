package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/herrors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func moduleDraftToAPI(value model.ModuleDraft) (ModuleDraft, error) {
	var definition ModuleVersionPublishBody
	if err := json.Unmarshal(value.Content, &definition); err != nil {
		return ModuleDraft{}, errors.Wrap(err, "failed to decode stored Draft definition")
	}
	return ModuleDraft{Id: value.ID, OrgId: value.OrgID, ModuleUuid: value.ModuleUUID, ModuleSlug: value.ModuleSlug,
		ResourceVersion: value.ResourceVersion, Definition: definition, Status: ModuleDraftStatus(value.Status),
		PublishedVersionUuid: value.PublishedVersionUUID, CreatedAt: value.CreatedAt, CreatedBy: value.CreatedBy,
		UpdatedAt: value.UpdatedAt, UpdatedBy: value.UpdatedBy}, nil
}

func moduleDraftError(err error) error {
	if bad, ok := model.IsErrBadRequest(err); ok {
		result := herrors.NewWithStatusAndDetails(http.StatusBadRequest, bad.Message, bad.Details, nil)
		if bad.Code != "" {
			result.Code = bad.Code
		}
		return result
	}
	if missing, ok := model.IsErrNotFound(err); ok {
		return herrors.NewWithStatus(http.StatusNotFound, missing.Message, nil)
	}
	if conflict, ok := model.IsErrConflict(err); ok {
		return herrors.NewWithStatus(http.StatusConflict, conflict.Message, nil)
	}
	return err
}

func (s *Server) authorizeModuleAuthoring(ctx context.Context, org, permission string) (uuid.UUID, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return actor, s.checkOrgAuthorization(ctx, actor, org, permission)
}

func (s *Server) GetModuleDraft(ctx context.Context, request GetModuleDraftRequestObject) (GetModuleDraftResponseObject, error) {
	if _, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	value, err := s.Database.GetModuleDraft(ctx, nil, request.OrgId, request.ModuleId, request.DraftId, model.GetModeDefault)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	response, err := moduleDraftToAPI(*value)
	return GetModuleDraft200JSONResponse(response), err
}

func (s *Server) GetModuleDraftRevision(ctx context.Context, request GetModuleDraftRevisionRequestObject) (GetModuleDraftRevisionResponseObject, error) {
	if _, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	value, err := s.Database.GetModuleDraftRevision(ctx, nil, request.OrgId, request.ModuleId, request.DraftId, request.DraftRevision)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	var definition ModuleVersionPublishBody
	if err := json.Unmarshal(value.Content, &definition); err != nil {
		return nil, errors.Wrap(err, "failed to decode stored Draft revision")
	}
	return GetModuleDraftRevision200JSONResponse{DraftId: value.DraftID, ResourceVersion: value.ResourceVersion,
		Definition: definition, Status: ModuleDraftRevisionStatus(value.Status), PublishedVersionUuid: value.PublishedVersionUUID,
		CreatedAt: value.CreatedAt, Actor: value.Actor, Reason: value.Reason}, nil
}

func (s *Server) ListModuleDrafts(ctx context.Context, request ListModuleDraftsRequestObject) (ListModuleDraftsResponseObject, error) {
	if _, err := s.authorizeModuleAuthoring(ctx, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	after := uuid.Nil
	if request.Params.Page != nil {
		parsed, err := uuid.Parse(*request.Params.Page)
		if err != nil {
			return nil, herrors.NewWithStatus(http.StatusBadRequest, "invalid Draft page token", nil)
		}
		after = parsed
	}
	limit := ref.DerefOr(request.Params.PerPage, 50)
	if limit < 1 || limit > 100 {
		return nil, herrors.NewWithStatus(http.StatusBadRequest, "invalid Draft page size", nil)
	}
	values, err := s.Database.ListModuleDrafts(ctx, nil, request.OrgId, request.ModuleId, after, limit+1)
	if err != nil {
		return nil, moduleDraftError(err)
	}
	response := ModuleDraftPage{Items: make([]ModuleDraft, 0)}
	if len(values) > limit {
		values = values[:limit]
		response.NextPageToken = ref.Ref(values[len(values)-1].ID.String())
	}
	for _, value := range values {
		draft, err := moduleDraftToAPI(value)
		if err != nil {
			return nil, err
		}
		response.Items = append(response.Items, draft)
	}
	return ListModuleDrafts200JSONResponse(response), nil
}

// Each authoring command shares the existing command receipt transaction and
// rechecks its permission before returning a cached response.
func runModuleDraftCommand[T any](ctx context.Context, s *Server, org, scope, key string, body any, action func(model.Tx, uuid.UUID) (T, error)) (T, error) {
	var zero T
	actor, err := s.authorizeModuleAuthoring(ctx, org, PermissionModuleVersionPublish)
	if err != nil {
		return zero, err
	}
	fingerprint := commandIdentity(scope, body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[T](ctx, s.Database, tx, org, scope, key, fingerprint); err != nil {
		return zero, moduleDraftError(err)
	} else if found {
		return response, nil
	}
	response, err := action(tx, actor)
	if err != nil {
		return zero, moduleDraftError(err)
	}
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, org, scope, key, fingerprint, actor, response); err != nil {
		return zero, moduleDraftError(err)
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return response, nil
}

func draftContent(body ModuleVersionPublishBody) (json.RawMessage, error) {
	if err := validateModuleAuthoringBody(body, false); err != nil {
		return nil, model.NewErrBadRequest(err.Error())
	}
	return json.Marshal(body)
}

func (s *Server) CreateModuleDraft(ctx context.Context, request CreateModuleDraftRequestObject) (CreateModuleDraftResponseObject, error) {
	response, err := runModuleDraftCommand(ctx, s, request.OrgId, "draft-create:"+request.ModuleId, request.Params.IdempotencyKey, request.Body,
		func(tx model.Tx, actor uuid.UUID) (ModuleDraft, error) {
			content, err := draftContent(request.Body.Definition)
			if err != nil {
				return ModuleDraft{}, err
			}
			draft, err := s.Database.CreateModuleDraft(ctx, tx, request.OrgId, request.ModuleId, content, actor, request.Body.Reason)
			if err != nil {
				return ModuleDraft{}, err
			}
			return moduleDraftToAPI(*draft)
		})
	return CreateModuleDraft201JSONResponse(response), err
}

func (s *Server) UpdateModuleDraft(ctx context.Context, request UpdateModuleDraftRequestObject) (UpdateModuleDraftResponseObject, error) {
	response, err := runModuleDraftCommand(ctx, s, request.OrgId, "draft-save:"+request.DraftId.String(), request.Params.IdempotencyKey, request,
		func(tx model.Tx, actor uuid.UUID) (ModuleDraft, error) {
			content, err := draftContent(request.Body.Definition)
			if err != nil {
				return ModuleDraft{}, err
			}
			draft, err := s.Database.UpdateModuleDraft(ctx, tx, request.OrgId, request.ModuleId, request.DraftId,
				request.Body.ExpectedResourceVersion, content, actor, request.Body.Reason)
			if err != nil {
				return ModuleDraft{}, err
			}
			return moduleDraftToAPI(*draft)
		})
	return UpdateModuleDraft200JSONResponse(response), err
}

func (s *Server) DiscardModuleDraft(ctx context.Context, request DiscardModuleDraftRequestObject) (DiscardModuleDraftResponseObject, error) {
	response, err := runModuleDraftCommand(ctx, s, request.OrgId, "draft-discard:"+request.DraftId.String(), request.Params.IdempotencyKey, request,
		func(tx model.Tx, actor uuid.UUID) (ModuleDraft, error) {
			draft, err := s.Database.CloseModuleDraft(ctx, tx, request.OrgId, request.ModuleId, request.DraftId,
				request.Body.ExpectedResourceVersion, nil, actor, request.Body.Reason)
			if err != nil {
				return ModuleDraft{}, err
			}
			return moduleDraftToAPI(*draft)
		})
	return DiscardModuleDraft200JSONResponse(response), err
}

func (s *Server) PublishModuleDraft(ctx context.Context, request PublishModuleDraftRequestObject) (PublishModuleDraftResponseObject, error) {
	response, err := runModuleDraftCommand(ctx, s, request.OrgId, "draft-publish:"+request.DraftId.String(), request.Params.IdempotencyKey, request,
		func(tx model.Tx, actor uuid.UUID) (ModuleDraftPublication, error) {
			module, err := s.Database.GetModuleCatalogue(ctx, tx, request.OrgId, request.ModuleId, model.GetModeForUpdate)
			if err != nil {
				return ModuleDraftPublication{}, err
			}
			draft, err := s.Database.GetModuleDraft(ctx, tx, request.OrgId, request.ModuleId, request.DraftId, model.GetModeForUpdate)
			if err != nil {
				return ModuleDraftPublication{}, err
			}
			if draft.Status != "open" || draft.ResourceVersion != request.Body.ExpectedResourceVersion {
				return ModuleDraftPublication{}, model.NewErrConflict("Draft changed since review or is already closed")
			}
			var body ModuleVersionPublishBody
			if err := json.Unmarshal(draft.Content, &body); err != nil {
				return ModuleDraftPublication{}, errors.Wrap(err, "failed to decode stored Draft definition")
			}
			if err := validatePublicationBody(body); err != nil {
				return ModuleDraftPublication{}, model.NewErrBadRequest(err.Error())
			}
			if missing, err := s.checkTypesExist(ctx, tx, request.OrgId, publicationReferencedTypes(module, body)); err != nil {
				return ModuleDraftPublication{}, err
			} else if len(missing) > 0 {
				return ModuleDraftPublication{}, model.NewErrConflict(fmt.Sprintf("referenced Resource Types do not exist: %v", missing))
			}
			definition := publicationModel(module, body, actor)
			published, err := s.Database.PublishCoreModuleVersion(ctx, tx, &definition, actor)
			if err != nil {
				return ModuleDraftPublication{}, err
			}
			closed, err := s.Database.CloseModuleDraft(ctx, tx, request.OrgId, request.ModuleId, request.DraftId,
				draft.ResourceVersion, &published.UUID, actor, request.Body.Reason)
			if err != nil {
				return ModuleDraftPublication{}, err
			}
			result, err := moduleDraftToAPI(*closed)
			return ModuleDraftPublication{Draft: result, Version: coreModuleVersionToAPI(*published)}, err
		})
	return PublishModuleDraft201JSONResponse(response), err
}
