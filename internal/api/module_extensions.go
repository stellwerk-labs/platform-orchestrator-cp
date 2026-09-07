package api

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func nonEmptyString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func moduleOperationReservationToAPI(value model.ModuleOperationReservation) ModuleOperationReservation {
	return ModuleOperationReservation{
		Id: value.ID, ModuleUuid: value.ModuleUUID, Namespace: value.Namespace, OperationId: value.OperationID,
		RelatedResourceId: value.RelatedResourceID, Reason: value.Reason, ResourceVersion: value.ResourceVersion,
		AcquiredBy: value.AcquiredBy, AcquiredAt: value.AcquiredAt, ReleasedBy: value.ReleasedBy,
		ReleasedAt: value.ReleasedAt, ReleaseReason: nonEmptyString(value.ReleaseReason),
	}
}

func moduleExtensionContributionToAPI(value model.ModuleExtensionContribution) ModuleExtensionContribution {
	return ModuleExtensionContribution{
		Id: value.ID, ModuleUuid: value.ModuleUUID, VersionUuid: value.VersionUUID,
		EnvironmentUuid: value.EnvironmentUUID, Namespace: value.Namespace,
		ExternalResourceId: value.ExternalResourceID, Kind: ModuleExtensionContributionKind(value.Kind),
		LifecycleState: ModuleExtensionContributionLifecycleState(value.LifecycleState), Label: value.Label,
		TargetUrl: nonEmptyString(value.TargetURL), Payload: value.Payload, CreatedBy: value.CreatedBy,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func (s *Server) ListModuleOperationReservations(ctx context.Context, request ListModuleOperationReservationsRequestObject) (ListModuleOperationReservationsResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	module, err := s.Database.GetModuleCatalogue(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ListModuleOperationReservations404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	reservations, err := s.Database.ListModuleOperationReservations(ctx, nil, request.OrgId, module.UUID, ref.DerefOr(request.Params.IncludeReleased, false))
	if err != nil {
		return nil, err
	}
	response := make(ListModuleOperationReservations200JSONResponse, 0, len(reservations))
	for _, reservation := range reservations {
		response = append(response, moduleOperationReservationToAPI(reservation))
	}
	return response, nil
}

func (s *Server) AcquireModuleOperationReservation(ctx context.Context, request AcquireModuleOperationReservationRequestObject) (AcquireModuleOperationReservationResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleArchive); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("module-reservation:%s:%s:%s", request.ModuleId, request.Body.Namespace, request.Body.OperationId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleOperationReservation](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return AcquireModuleOperationReservation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return AcquireModuleOperationReservation201JSONResponse(response), nil
	}
	reservation, err := s.Database.AcquireModuleOperationReservation(ctx, tx, request.OrgId, request.ModuleId,
		request.Body.Namespace, request.Body.OperationId, request.Body.RelatedResourceId, request.Body.Reason, actor)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return AcquireModuleOperationReservation400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return AcquireModuleOperationReservation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return AcquireModuleOperationReservation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := moduleOperationReservationToAPI(*reservation)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return AcquireModuleOperationReservation201JSONResponse(response), nil
}

func (s *Server) ReleaseModuleOperationReservation(ctx context.Context, request ReleaseModuleOperationReservationRequestObject) (ReleaseModuleOperationReservationResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleArchive); err != nil {
		return nil, err
	}
	module, err := s.Database.GetModuleCatalogue(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ReleaseModuleOperationReservation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	reservation, err := s.Database.GetModuleOperationReservation(ctx, nil, request.OrgId, request.ReservationId, model.GetModeDefault)
	if err != nil || reservation.ModuleUUID != module.UUID {
		if err == nil {
			err = model.NewErrNotFound("Module operation reservation not found")
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ReleaseModuleOperationReservation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	scope := fmt.Sprintf("module-reservation-release:%s", request.ReservationId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleOperationReservation](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return ReleaseModuleOperationReservation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return ReleaseModuleOperationReservation200JSONResponse(response), nil
	}
	released, err := s.Database.ReleaseModuleOperationReservation(ctx, tx, request.OrgId, request.ReservationId,
		request.Body.ExpectedResourceVersion, actor, request.Body.Reason)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return ReleaseModuleOperationReservation400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ReleaseModuleOperationReservation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return ReleaseModuleOperationReservation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := moduleOperationReservationToAPI(*released)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ReleaseModuleOperationReservation200JSONResponse(response), nil
}

func (s *Server) ListModuleExtensionContributions(ctx context.Context, request ListModuleExtensionContributionsRequestObject) (ListModuleExtensionContributionsResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	module, err := s.Database.GetModuleCatalogue(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ListModuleExtensionContributions404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	contributions, err := s.Database.ListModuleExtensionContributions(ctx, nil, request.OrgId, module.UUID,
		ref.DerefOr(request.Params.IncludeDraft, false), ref.DerefOr(request.Params.IncludeTerminal, false))
	if err != nil {
		return nil, err
	}
	response := make(ListModuleExtensionContributions200JSONResponse, 0, len(contributions))
	for _, contribution := range contributions {
		response = append(response, moduleExtensionContributionToAPI(contribution))
	}
	return response, nil
}

func (s *Server) UpsertModuleExtensionContribution(ctx context.Context, request UpsertModuleExtensionContributionRequestObject) (UpsertModuleExtensionContributionResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, actor, request.OrgId, PermissionModuleArchive); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("module-extension:%s:%s:%s:%s", request.ModuleId, request.Body.Namespace, request.Body.Kind, request.Body.ExternalResourceId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleExtensionContribution](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return UpsertModuleExtensionContribution409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return UpsertModuleExtensionContribution200JSONResponse(response), nil
	}
	contribution, err := s.Database.UpsertModuleExtensionContribution(ctx, tx, request.OrgId, request.ModuleId,
		request.Body.VersionUuid, request.Body.EnvironmentUuid, request.Body.Namespace, request.Body.ExternalResourceId, string(request.Body.Kind),
		string(request.Body.LifecycleState), request.Body.Label, ref.DerefOr(request.Body.TargetUrl, ""), request.Body.Payload, actor)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return UpsertModuleExtensionContribution400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return UpsertModuleExtensionContribution404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return UpsertModuleExtensionContribution409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, errors.Wrap(err, "failed to register Module extension contribution")
	}
	response := moduleExtensionContributionToAPI(*contribution)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return UpsertModuleExtensionContribution200JSONResponse(response), nil
}
