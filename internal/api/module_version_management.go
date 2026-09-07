package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
)

func moduleCatalogueToAPI(value model.ModuleCatalogue) ModuleCatalogueEntry {
	return ModuleCatalogueEntry{
		OrgId: value.OrgID, Uuid: value.UUID, Slug: value.Slug, DisplayName: value.DisplayName,
		Description: value.Description, ResourceType: value.ResourceType, Tags: value.Tags,
		Status: ModuleCatalogueStatus(value.Status), ResourceVersion: value.ResourceVersion,
		CurrentDefaultVersionUuid: value.CurrentDefaultVersionUUID, PreviousDefaultVersionUuid: value.PreviousDefaultVersionUUID,
		ManagedDefaultGeneration: value.ManagedDefaultGeneration, CreatedAt: value.CreatedAt, ArchivedAt: value.ArchivedAt,
		ArchivedBy: value.ArchivedBy, ArchiveReason: ref.RefStringEmptyNil(value.ArchiveReason),
	}
}

func coreModuleVersionToAPI(value model.CoreModuleVersion) CoreModuleVersion {
	return CoreModuleVersion{
		OrgId: value.OrgID, ModuleUuid: value.ModuleUUID, ModuleSlug: value.ModuleSlug, Uuid: value.UUID,
		SemanticVersion: value.SemanticVersion, OpaqueVersionId: value.OpaqueVersionID,
		MigrationGeneration: CoreModuleVersionMigrationGeneration(value.MigrationGeneration), ArtifactDigest: value.ArtifactDigest,
		VerificationStatus: ModuleVerificationStatus(value.VerificationStatus), LifecycleStatus: ModuleVersionSemanticStatus(value.LifecycleStatus),
		SourceRevision: value.SourceRevision, ReleaseNotes: value.ReleaseNotes, ResourceVersion: value.ResourceVersion,
		PublishedBy: value.PublishedBy, CreatedAt: value.CreatedAt,
	}
}

func modulePinToAPI(value model.EnvironmentModuleVersionPin) EnvironmentModuleVersionPin {
	return EnvironmentModuleVersionPin{
		Id: value.ID, OrgId: value.OrgID, ProjectUuid: value.ProjectUUID, ProjectId: value.ProjectID,
		EnvironmentUuid: value.EnvironmentUUID, EnvironmentId: value.EnvironmentID,
		ModuleUuid: value.ModuleUUID, VersionUuid: value.VersionUUID, Status: ModuleVersionPinStatus(value.Status),
		ResourceVersion: value.ResourceVersion, ActivationEventId: value.ActivationEventID, BulkOperationId: value.BulkOperationID,
		OverrideOperationId: value.OverrideOperationID, OverrideTargetVersionUuid: value.OverrideTargetVersionUUID,
		OverrideActor: value.OverrideActor, OverrideReason: ref.RefStringEmptyNil(value.OverrideReason),
		OverrideDeploymentId: value.OverrideDeploymentID, CreatedBy: value.CreatedBy, CreatedAt: value.CreatedAt,
		UpdatedAt: value.UpdatedAt, RemovedAt: value.RemovedAt,
	}
}

func (s *Server) CreateModuleCatalogueEntry(ctx context.Context, request CreateModuleCatalogueEntryRequestObject) (CreateModuleCatalogueEntryResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleWrite); err != nil {
		return nil, err
	}
	resourceType, err := s.Database.GetResourceType(ctx, nil, &request.OrgId, request.Body.ResourceType)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CreateModuleCatalogueEntry404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if resourceType.CatalogueStatus != string(ResourceTypeCatalogueStatusActive) {
		return CreateModuleCatalogueEntry409JSONResponse{N409ConflictJSONResponse: Generate409Response("archived resource types reject new Module bindings")}, nil
	}
	scope := fmt.Sprintf("module-create:%s", request.Body.Slug)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleCatalogueEntry](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return CreateModuleCatalogueEntry409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return CreateModuleCatalogueEntry201JSONResponse(response), nil
	}
	entry, err := s.Database.CreateEmptyModule(ctx, tx, request.OrgId, request.Body.Slug,
		ref.DerefOr(request.Body.DisplayName, request.Body.Slug), ref.DerefOr(request.Body.Description, ""),
		request.Body.ResourceType, ref.DerefOr(request.Body.Tags, map[string]string{}))
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CreateModuleCatalogueEntry404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return CreateModuleCatalogueEntry409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := moduleCatalogueToAPI(*entry)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return CreateModuleCatalogueEntry201JSONResponse(response), nil
}

func (s *Server) ListModuleCatalogueEntries(ctx context.Context, request ListModuleCatalogueEntriesRequestObject) (ListModuleCatalogueEntriesResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleCoreRead); err != nil {
		return nil, err
	}
	entries, err := s.Database.ListModuleCatalogues(ctx, nil, request.OrgId, ref.DerefOr(request.Params.IncludeArchived, false))
	if err != nil {
		return nil, err
	}
	response := make(ListModuleCatalogueEntries200JSONResponse, 0, len(entries))
	for _, entry := range entries {
		response = append(response, moduleCatalogueToAPI(entry))
	}
	return response, nil
}

func (s *Server) GetModuleCatalogueEntry(ctx context.Context, request GetModuleCatalogueEntryRequestObject) (GetModuleCatalogueEntryResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleCoreRead); err != nil {
		return nil, err
	}
	entry, err := s.Database.GetModuleCatalogue(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return GetModuleCatalogueEntry404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	return GetModuleCatalogueEntry200JSONResponse(moduleCatalogueToAPI(*entry)), nil
}

func (s *Server) UpdateModuleCatalogueEntry(ctx context.Context, request UpdateModuleCatalogueEntryRequestObject) (UpdateModuleCatalogueEntryResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleWrite); err != nil {
		return nil, err
	}
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	entry, err := s.Database.UpdateModuleCatalogueMetadata(ctx, tx, request.OrgId, request.ModuleId,
		strings.TrimSpace(request.Body.DisplayName), request.Body.Description, request.Body.Tags, request.Body.ExpectedResourceVersion)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return UpdateModuleCatalogueEntry404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return UpdateModuleCatalogueEntry409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return UpdateModuleCatalogueEntry200JSONResponse(moduleCatalogueToAPI(*entry)), nil
}

func (s *Server) ChangeModuleCatalogueStatus(ctx context.Context, request ChangeModuleCatalogueStatusRequestObject) (ChangeModuleCatalogueStatusResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleArchive); err != nil {
		return nil, err
	}
	target := moduleversions.CatalogueArchived
	if request.CatalogueAction == ChangeModuleCatalogueStatusParamsCatalogueActionUnarchive {
		target = moduleversions.CatalogueActive
	}
	scope := fmt.Sprintf("catalogue:%s:%s", request.ModuleId, request.CatalogueAction)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleCatalogueEntry](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return ChangeModuleCatalogueStatus409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return ChangeModuleCatalogueStatus200JSONResponse(response), nil
	}
	entry, err := s.Database.SetModuleCatalogueStatus(ctx, tx, request.OrgId, request.ModuleId, target, userID,
		request.Body.Reason, request.Body.ExpectedResourceVersion)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return ChangeModuleCatalogueStatus400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ChangeModuleCatalogueStatus404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return ChangeModuleCatalogueStatus409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := moduleCatalogueToAPI(*entry)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ChangeModuleCatalogueStatus200JSONResponse(response), nil
}

func publicationModel(module *model.ModuleCatalogue, body ModuleVersionPublishBody, actor uuid.UUID) model.ModuleDefinitionVersion {
	params := make(map[string]model.ModuleParam, len(body.ModuleParams))
	for key, value := range body.ModuleParams {
		params[key] = model.ModuleParam{Type: string(value.Type), IsOptional: value.IsOptional, Description: ref.DerefOr(value.Description, "")}
	}
	dependencies := make(map[string]model.ModuleDefinitionDependency, len(body.Dependencies))
	for key, value := range body.Dependencies {
		dependencies[key] = dbMddFromApiMdd(value)
	}
	coprovisioned := make([]model.ModuleDefinitionCoProvision, 0, len(body.Coprovisioned))
	for _, value := range body.Coprovisioned {
		coprovisioned = append(coprovisioned, dbMdcFromApiMdc(value))
	}
	now := time.Now().UTC()
	return model.ModuleDefinitionVersion{
		OrgId: module.OrgID, DefinitionId: module.Slug, ModuleUUID: module.UUID, CreatedAt: module.CreatedAt,
		ResourceType: module.ResourceType, VersionId: body.SemanticVersion, SemanticVersion: body.SemanticVersion,
		UpdatedAt: now, Description: opt.OfRef(body.Description), ModuleSource: body.ModuleSource,
		ModuleSourceCode: opt.OfRef(body.ModuleSourceCode), ModuleInputs: body.ModuleInputs, ModuleParams: params,
		OutputSchema: ref.DerefOr(body.OutputSchema, nil),
		Dependencies: dependencies, CoProvisioned: coprovisioned, ProviderMapping: body.ProviderMapping,
		ArtifactDigest: ref.DerefOr(body.ArtifactDigest, ""), SourceRevision: ref.DerefOr(body.SourceRevision, ""),
		ReleaseNotes: opt.OfRef(body.ReleaseNotes), PublishedBy: &actor,
	}
}

func validatePublicationBody(body ModuleVersionPublishBody) error {
	if _, err := moduleversions.ParseVersion(body.SemanticVersion); err != nil {
		return err
	}
	if err := validateModuleSource(body.ModuleSource, body.ModuleSourceCode); err != nil {
		return err
	}
	if body.ModuleSource == inlineModuleSource {
		if body.ArtifactDigest != nil {
			return fmt.Errorf("artifact_digest protects referenced external artifacts and must be omitted for inline source")
		}
	} else {
		if body.ArtifactDigest != nil {
			if err := moduleversions.ValidateArtifactDigest(*body.ArtifactDigest); err != nil {
				return err
			}
		}
		if strings.TrimSpace(ref.DerefOr(body.SourceRevision, "")) == "" {
			return fmt.Errorf("source_revision is required for an external module artifact")
		}
	}
	if err := validateModuleInputsAndParamInputs(maps.Keys(body.ModuleInputs), maps.Keys(body.ModuleParams)); err != nil {
		return err
	}
	return ValidatePlaceholderSyntax(body.ModuleInputs, PlaceholdersSupportedInModule)
}

func publicationReferencedTypes(module *model.ModuleCatalogue, body ModuleVersionPublishBody) []string {
	types := make([]string, 0, 1+len(body.Dependencies)+len(body.Coprovisioned))
	types = append(types, module.ResourceType)
	for _, dependency := range body.Dependencies {
		types = append(types, dependency.Type)
	}
	for _, coprovisioned := range body.Coprovisioned {
		types = append(types, coprovisioned.Type)
	}
	return types
}

func (s *Server) PublishModuleVersion(ctx context.Context, request PublishModuleVersionRequestObject) (PublishModuleVersionResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionPublish); err != nil {
		return nil, err
	}
	if err := validatePublicationBody(*request.Body); err != nil {
		return PublishModuleVersion400JSONResponse{N400BadRequestJSONResponse: Generate400Response(err.Error())}, nil
	}
	scope := fmt.Sprintf("publish:%s", request.ModuleId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[CoreModuleVersion](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishModuleVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return PublishModuleVersion201JSONResponse(response), nil
	}
	module, err := s.Database.GetModuleCatalogue(ctx, tx, request.OrgId, request.ModuleId, model.GetModeForUpdate)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return PublishModuleVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if missingTypes, err := s.checkTypesExist(ctx, tx, request.OrgId, publicationReferencedTypes(module, *request.Body)); err != nil {
		return nil, errors.Wrap(err, "failed to validate referenced Resource Types")
	} else if len(missingTypes) > 0 {
		return PublishModuleVersion409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf(
			"the following Resource Types referenced by the Module Version do not exist: %v", missingTypes,
		))}, nil
	}
	definition := publicationModel(module, *request.Body, userID)
	version, err := s.Database.PublishCoreModuleVersion(ctx, tx, &definition, userID)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return PublishModuleVersion400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return PublishModuleVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishModuleVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := coreModuleVersionToAPI(*version)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return PublishModuleVersion201JSONResponse(response), nil
}

func lifecycleAction(action TransitionModuleVersionParamsLifecycleAction) (moduleversions.LifecycleStatus, string) {
	switch action {
	case TransitionModuleVersionParamsLifecycleActionPromote, TransitionModuleVersionParamsLifecycleActionRestore:
		return moduleversions.LifecycleDefault, PermissionModuleVersionPromote
	case TransitionModuleVersionParamsLifecycleActionDeprecate:
		return moduleversions.LifecycleDeprecated, PermissionModuleVersionDeprecate
	case TransitionModuleVersionParamsLifecycleActionMarkDefective:
		return moduleversions.LifecycleDefective, PermissionModuleVersionDefective
	default:
		return "", ""
	}
}

func transactionLifecycleAction(action ModuleVersionLifecycleTransactionItemAction) (moduleversions.LifecycleStatus, string) {
	switch action {
	case ModuleVersionLifecycleTransactionItemActionPromote:
		return moduleversions.LifecycleDefault, PermissionModuleVersionPromote
	case ModuleVersionLifecycleTransactionItemActionRestore:
		return moduleversions.LifecycleDefault, PermissionModuleVersionRestore
	case ModuleVersionLifecycleTransactionItemActionDeprecate:
		return moduleversions.LifecycleDeprecated, PermissionModuleVersionDeprecate
	case ModuleVersionLifecycleTransactionItemActionMarkDefective:
		return moduleversions.LifecycleDefective, PermissionModuleVersionDefective
	default:
		return "", ""
	}
}

func (s *Server) TransitionModuleVersion(ctx context.Context, request TransitionModuleVersionRequestObject) (TransitionModuleVersionResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	target, permission := lifecycleAction(request.LifecycleAction)
	if request.LifecycleAction == TransitionModuleVersionParamsLifecycleActionRestore {
		permission = PermissionModuleVersionRestore
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, permission); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("lifecycle:%s:%s:%s", request.ModuleId, request.ModuleVersionId, request.LifecycleAction)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[CoreModuleVersion](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return TransitionModuleVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return TransitionModuleVersion200JSONResponse(response), nil
	}
	version, err := s.Database.TransitionCoreModuleVersion(ctx, tx, request.OrgId, request.ModuleId, request.ModuleVersionId,
		target, request.Body.ExpectedResourceVersion, userID, request.Body.Reason, nil)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return TransitionModuleVersion400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return TransitionModuleVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return TransitionModuleVersion409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := coreModuleVersionToAPI(*version)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return TransitionModuleVersion200JSONResponse(response), nil
}

func (s *Server) TransactModuleVersionLifecycles(ctx context.Context, request TransactModuleVersionLifecyclesRequestObject) (TransactModuleVersionLifecyclesResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	seenModules := make(map[string]struct{}, len(request.Body.Transitions))
	for _, transition := range request.Body.Transitions {
		if _, duplicate := seenModules[transition.ModuleId]; duplicate {
			return TransactModuleVersionLifecycles400JSONResponse{N400BadRequestJSONResponse: Generate400Response("a lifecycle transaction may change each Module at most once")}, nil
		}
		seenModules[transition.ModuleId] = struct{}{}
		_, permission := transactionLifecycleAction(transition.Action)
		if permission == "" {
			return TransactModuleVersionLifecycles400JSONResponse{N400BadRequestJSONResponse: Generate400Response("unsupported lifecycle action")}, nil
		}
		if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, permission); err != nil {
			return nil, err
		}
	}
	scope := "lifecycle-transaction"
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleVersionLifecycleTransactionResult](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return TransactModuleVersionLifecycles409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return TransactModuleVersionLifecycles200JSONResponse(response), nil
	}
	transitions := slices.Clone(request.Body.Transitions)
	slices.SortFunc(transitions, func(left, right ModuleVersionLifecycleTransactionItem) int {
		return strings.Compare(left.ModuleId, right.ModuleId)
	})
	correlationID := uuid.New()
	versions := make([]CoreModuleVersion, 0, len(transitions))
	for _, transition := range transitions {
		target, _ := transactionLifecycleAction(transition.Action)
		version, err := s.Database.TransitionCoreModuleVersion(ctx, tx, request.OrgId, transition.ModuleId,
			transition.ModuleVersionId, target, transition.ExpectedResourceVersion, userID, transition.Reason, &correlationID)
		if err != nil {
			if badRequest, ok := model.IsErrBadRequest(err); ok {
				return TransactModuleVersionLifecycles400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
			}
			if notFound, ok := model.IsErrNotFound(err); ok {
				return TransactModuleVersionLifecycles404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
			}
			if conflict, ok := model.IsErrConflict(err); ok {
				return TransactModuleVersionLifecycles409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
			}
			return nil, err
		}
		versions = append(versions, coreModuleVersionToAPI(*version))
	}
	response := ModuleVersionLifecycleTransactionResult{CorrelationId: correlationID, Versions: versions}
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return TransactModuleVersionLifecycles200JSONResponse(response), nil
}

func (s *Server) ListModuleVersionLifecycleEvents(ctx context.Context, request ListModuleVersionLifecycleEventsRequestObject) (ListModuleVersionLifecycleEventsResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	events, err := s.Database.ListModuleLifecycleEvents(ctx, nil, request.OrgId, request.ModuleId, request.ModuleVersionId)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ListModuleVersionLifecycleEvents404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	response := make(ListModuleVersionLifecycleEvents200JSONResponse, 0, len(events))
	for _, event := range events {
		var from *ModuleVersionSemanticStatus
		if event.FromStatus != nil {
			value := ModuleVersionSemanticStatus(*event.FromStatus)
			from = &value
		}
		payload := map[string]interface{}{}
		_ = json.Unmarshal(event.Payload, &payload)
		response = append(response, ModuleVersionLifecycleEvent{
			Sequence: event.Sequence, Id: event.ID, ModuleUuid: event.ModuleUUID, VersionUuid: event.VersionUUID,
			FromStatus: from, ToStatus: ModuleVersionSemanticStatus(event.ToStatus), VersionResourceVersion: event.ResourceVersion,
			Actor: event.Actor, Reason: event.Reason, CorrelationId: event.CorrelationID, Payload: payload, CreatedAt: event.CreatedAt,
		})
	}
	return response, nil
}

func (s *Server) PublishStableModuleVersionSuccessor(ctx context.Context, request PublishStableModuleVersionSuccessorRequestObject) (PublishStableModuleVersionSuccessorResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionPublish); err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionDeprecate); err != nil {
		return nil, err
	}
	if err := validatePublicationBody(request.Body.Version); err != nil {
		return PublishStableModuleVersionSuccessor400JSONResponse{N400BadRequestJSONResponse: Generate400Response(err.Error())}, nil
	}
	scope := fmt.Sprintf("stable-successor:%s:%s", request.ModuleId, request.ModuleVersionId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[StableModuleVersionSuccessorResult](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishStableModuleVersionSuccessor409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return PublishStableModuleVersionSuccessor201JSONResponse(response), nil
	}
	module, err := s.Database.GetModuleCatalogue(ctx, tx, request.OrgId, request.ModuleId, model.GetModeForUpdate)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return PublishStableModuleVersionSuccessor404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if missingTypes, err := s.checkTypesExist(ctx, tx, request.OrgId, publicationReferencedTypes(module, request.Body.Version)); err != nil {
		return nil, errors.Wrap(err, "failed to validate referenced Resource Types")
	} else if len(missingTypes) > 0 {
		return PublishStableModuleVersionSuccessor409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf(
			"the following Resource Types referenced by the stable Module Version do not exist: %v", missingTypes,
		))}, nil
	}
	definition := publicationModel(module, request.Body.Version, userID)
	result, err := s.Database.PublishStableModuleVersionSuccessor(ctx, tx, request.OrgId, request.ModuleId,
		request.ModuleVersionId, request.Body.ExpectedPrereleaseResourceVersion, request.Body.Reason, &definition, userID)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return PublishStableModuleVersionSuccessor400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return PublishStableModuleVersionSuccessor404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return PublishStableModuleVersionSuccessor409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := StableModuleVersionSuccessorResult{
		Prerelease: coreModuleVersionToAPI(result.Prerelease), Stable: coreModuleVersionToAPI(result.Stable),
		CorrelationId: result.CorrelationID,
	}
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return PublishStableModuleVersionSuccessor201JSONResponse(response), nil
}

func structuralKeyDiff(left, right any) (added, removed, changed []string) {
	added = []string{}
	removed = []string{}
	changed = []string{}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Kind() != reflect.Map || rightValue.Kind() != reflect.Map {
		return added, removed, changed
	}
	for _, key := range leftValue.MapKeys() {
		name := key.String()
		rightItem := rightValue.MapIndex(key)
		if !rightItem.IsValid() {
			removed = append(removed, name)
		} else if !reflect.DeepEqual(leftValue.MapIndex(key).Interface(), rightItem.Interface()) {
			changed = append(changed, name)
		}
	}
	for _, key := range rightValue.MapKeys() {
		if !leftValue.MapIndex(key).IsValid() {
			added = append(added, key.String())
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return added, removed, changed
}

func moduleVersionComparisonSnapshot(value model.ModuleDefinitionVersion) ModuleVersionComparisonSnapshot {
	definition := apiMdFromDbMd(value)
	return ModuleVersionComparisonSnapshot{
		ModuleSource:     definition.ModuleSource,
		ModuleSourceCode: definition.ModuleSourceCode,
		ArtifactDigest:   value.ArtifactDigest,
		SourceRevision:   value.SourceRevision,
		ResourceType:     definition.ResourceType,
		ModuleInputs:     definition.ModuleInputs,
		ModuleParams:     definition.ModuleParams,
		ProviderMapping:  definition.ProviderMapping,
		Dependencies:     definition.Dependencies,
		Coprovisioned:    definition.Coprovisioned,
		OutputSchema:     optionalJSONObject[ModuleOutputSchema](value.OutputSchema),
	}
}

func (s *Server) CompareModuleVersions(ctx context.Context, request CompareModuleVersionsRequestObject) (CompareModuleVersionsResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	left, err := s.Database.GetModuleDefinitionVersion(ctx, nil, request.OrgId, request.ModuleId, request.ModuleVersionId)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CompareModuleVersions404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	right, err := s.Database.GetModuleDefinitionVersion(ctx, nil, request.OrgId, request.ModuleId, request.OtherModuleVersionId)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CompareModuleVersions404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	addedInputs, removedInputs, changedInputs := structuralKeyDiff(left.ModuleInputs, right.ModuleInputs)
	addedParams, removedParams, changedParams := structuralKeyDiff(left.ModuleParams, right.ModuleParams)
	addedProviders, removedProviders, changedProviders := structuralKeyDiff(left.ProviderMapping, right.ProviderMapping)
	addedDependencies, removedDependencies, changedDependencies := structuralKeyDiff(left.Dependencies, right.Dependencies)
	return CompareModuleVersions200JSONResponse(ModuleVersionComparison{
		FromVersionUuid: left.VersionUUID, ToVersionUuid: right.VersionUUID,
		Before: moduleVersionComparisonSnapshot(*left), After: moduleVersionComparisonSnapshot(*right),
		ModuleSourceChanged:     left.ModuleSource != right.ModuleSource,
		ModuleSourceCodeChanged: !reflect.DeepEqual(left.ModuleSourceCode.Ref(), right.ModuleSourceCode.Ref()),
		ArtifactDigestChanged:   left.ArtifactDigest != right.ArtifactDigest,
		SourceRevisionChanged:   left.SourceRevision != right.SourceRevision,
		ResourceTypeChanged:     left.ResourceType != right.ResourceType,
		AddedModuleInputs:       addedInputs, RemovedModuleInputs: removedInputs, ChangedModuleInputs: changedInputs,
		AddedModuleParams: addedParams, RemovedModuleParams: removedParams, ChangedModuleParams: changedParams,
		AddedProviderMappings: addedProviders, RemovedProviderMappings: removedProviders, ChangedProviderMappings: changedProviders,
		AddedDependencies: addedDependencies, RemovedDependencies: removedDependencies, ChangedDependencies: changedDependencies,
		CoprovisioningChanged: !reflect.DeepEqual(left.CoProvisioned, right.CoProvisioned),
		OutputSchemaChanged:   !reflect.DeepEqual(left.OutputSchema, right.OutputSchema),
	}), nil
}

type observedModuleUsage struct {
	Items      []observedModuleUsageItem `json:"items"`
	ObservedAt time.Time                 `json:"observed_at"`
}

type observedModuleUsageItem struct {
	ProjectID       string    `json:"project_id"`
	EnvironmentID   string    `json:"env_id"`
	EnvironmentUUID uuid.UUID `json:"environment_uuid"`
	ModuleVersion   string    `json:"module_version"`
	DeploymentID    uuid.UUID `json:"deployment_id"`
	ObservedAt      time.Time `json:"observed_at"`
}

func (s *Server) GetModuleVersionUsage(ctx context.Context, request GetModuleVersionUsageRequestObject) (GetModuleVersionUsageResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleCoreRead); err != nil {
		return nil, err
	}
	version, err := s.Database.GetCoreModuleVersion(ctx, nil, request.OrgId, request.ModuleId, request.ModuleVersionId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return GetModuleVersionUsage404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	dpResponse, err := s.DpClient.InternalCheckModuleUsageWithResponse(ctx, request.OrgId, version.ModuleSlug, &orchestratordp.InternalCheckModuleUsageParams{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve observed Module adoption")
	}
	if dpResponse.StatusCode() != http.StatusOK {
		return nil, errors.Errorf("unexpected status code %d while retrieving Module adoption", dpResponse.StatusCode())
	}
	var observed observedModuleUsage
	if err := json.Unmarshal(dpResponse.Body, &observed); err != nil {
		return nil, errors.Wrap(err, "failed to decode observed Module adoption")
	}

	environments := make(map[string]model.Environment)
	pageToken := ""
	for {
		page, next, err := s.Database.ListEnvironmentsInOrg(ctx, nil, request.OrgId, pageToken, 100, model.ListEnvironmentsParams{})
		if err != nil {
			return nil, err
		}
		for _, environment := range page {
			environments[environment.ProjectId+"\x00"+environment.Id] = environment
		}
		if next == "" {
			break
		}
		pageToken = next
	}

	response := ModuleVersionUsage{
		ModuleUuid: version.ModuleUUID, VersionUuid: version.UUID,
		SemanticVersion: ref.DerefOr(version.SemanticVersion, version.OpaqueVersionID),
		ByProject:       map[string]int{}, ByEnvironmentType: map[string]int{}, Environments: []ModuleVersionUsageEnvironment{},
		UnknownEnvironments: []uuid.UUID{}, ObservedAt: observed.ObservedAt,
	}
	knownModuleEnvironments := make(map[string]struct{})
	for _, item := range observed.Items {
		key := item.ProjectID + "\x00" + item.EnvironmentID
		knownModuleEnvironments[key] = struct{}{}
		if item.ModuleVersion != version.OpaqueVersionID {
			continue
		}
		environment, exists := environments[key]
		if !exists {
			continue
		}
		response.ActiveEnvironmentCount++
		response.ByProject[item.ProjectID]++
		response.ByEnvironmentType[environment.EnvTypeId]++
		response.Environments = append(response.Environments, ModuleVersionUsageEnvironment{
			ProjectId: item.ProjectID, EnvironmentId: item.EnvironmentID, EnvironmentUuid: environment.Uuid,
			EnvironmentType: environment.EnvTypeId, DeploymentId: item.DeploymentID, ObservedAt: item.ObservedAt,
		})
	}
	for key, environment := range environments {
		if _, known := knownModuleEnvironments[key]; !known {
			response.UnknownEnvironments = append(response.UnknownEnvironments, environment.Uuid)
		}
	}
	pins, err := s.Database.ListEnvironmentModuleVersionPins(ctx, nil, request.OrgId, nil, &version.ModuleUUID, true)
	if err != nil {
		return nil, err
	}
	for _, pin := range pins {
		if pin.VersionUUID != version.UUID {
			continue
		}
		switch pin.Status {
		case moduleversions.PinActive:
			response.ActivePins++
		case moduleversions.PinOverridePending:
			response.OverridePendingPins++
		default:
			response.HistoricalPins++
		}
	}
	return GetModuleVersionUsage200JSONResponse(response), nil
}

func (s *Server) ListEnvironmentModuleVersionPins(ctx context.Context, request ListEnvironmentModuleVersionPinsRequestObject) (ListEnvironmentModuleVersionPinsResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	if request.Params.EnvironmentUuid == nil {
		if err := s.checkOrgAuthorization(ctx, userID, request.OrgId, PermissionModuleVersionRead); err != nil {
			return nil, err
		}
	} else if err := s.checkPinScopeAuthorization(ctx, userID, request.OrgId, *request.Params.EnvironmentUuid, uuid.Nil, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	pins, err := s.Database.ListEnvironmentModuleVersionPins(ctx, nil, request.OrgId, request.Params.EnvironmentUuid,
		request.Params.ModuleUuid, ref.DerefOr(request.Params.IncludeRemoved, false))
	if err != nil {
		return nil, err
	}
	response := make(ListEnvironmentModuleVersionPins200JSONResponse, 0, len(pins))
	for _, pin := range pins {
		response = append(response, modulePinToAPI(pin))
	}
	return response, nil
}

func (s *Server) checkPinScopeAuthorization(ctx context.Context, userID uuid.UUID, orgID string, environmentUUID, projectUUID uuid.UUID, permission string) error {
	checks := make([]orchestratoriam.ResourcePermissionCheck, 0, 2)
	if environmentUUID != uuid.Nil {
		checks = append(checks, environmentCheck(environmentUUID, permission))
	}
	if projectUUID != uuid.Nil {
		checks = append(checks, projectCheck(projectUUID, permission))
	}
	for _, check := range checks {
		if err := s.innerCheck(ctx, userID, orgID, []orchestratoriam.ResourcePermissionCheck{check}); err == nil {
			return nil
		}
	}
	return s.checkOrgAuthorization(ctx, userID, orgID, permission)
}

func (s *Server) GetEnvironmentModuleVersionPin(ctx context.Context, request GetEnvironmentModuleVersionPinRequestObject) (GetEnvironmentModuleVersionPinResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return GetEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, userID, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	return GetEnvironmentModuleVersionPin200JSONResponse(modulePinToAPI(*pin)), nil
}

func (s *Server) ListEnvironmentModuleVersionPinEvents(ctx context.Context, request ListEnvironmentModuleVersionPinEventsRequestObject) (ListEnvironmentModuleVersionPinEventsResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ListEnvironmentModuleVersionPinEvents404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, userID, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionRead); err != nil {
		return nil, err
	}
	events, err := s.Database.ListModuleVersionPinEvents(ctx, nil, request.OrgId, request.PinId)
	if err != nil {
		return nil, err
	}
	response := make(ListEnvironmentModuleVersionPinEvents200JSONResponse, 0, len(events))
	for _, event := range events {
		response = append(response, modulePinEventToAPI(event))
	}
	return response, nil
}

func modulePinEventToAPI(event model.ModuleVersionPinEvent) ModuleVersionPinEvent {
	var from *ModuleVersionPinStatus
	if event.FromStatus != nil {
		value := ModuleVersionPinStatus(*event.FromStatus)
		from = &value
	}
	var reason, note *string
	if event.EventType == "note_added" {
		note = &event.Reason
	} else {
		reason = &event.Reason
	}
	return ModuleVersionPinEvent{
		Sequence: event.Sequence, Id: event.ID, PinId: event.PinID, Revision: event.Revision,
		EventType: event.EventType, FromStatus: from, ToStatus: ModuleVersionPinStatus(event.ToStatus),
		ActivationEventId: event.ActivationEventID, Actor: event.Actor, ActorType: event.ActorType,
		Reason: reason, Note: note, OperationId: event.OperationID, DeploymentId: event.DeploymentID,
		BulkOperationId: event.BulkOperationID, CreatedAt: event.CreatedAt,
	}
}

func (s *Server) AppendEnvironmentModuleVersionPinNote(ctx context.Context, request AppendEnvironmentModuleVersionPinNoteRequestObject) (AppendEnvironmentModuleVersionPinNoteResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return AppendEnvironmentModuleVersionPinNote404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, actor, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionPinNote); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("pin-note:%s:%s", request.PinId, actor)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleVersionPinEvent](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return AppendEnvironmentModuleVersionPinNote409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return AppendEnvironmentModuleVersionPinNote201JSONResponse(response), nil
	}
	event, err := s.Database.AppendModuleVersionPinNote(ctx, tx, request.OrgId, request.PinId, actor, "user", request.Body.Note)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return AppendEnvironmentModuleVersionPinNote400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return AppendEnvironmentModuleVersionPinNote404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return AppendEnvironmentModuleVersionPinNote409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinEventToAPI(*event)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return AppendEnvironmentModuleVersionPinNote201JSONResponse(response), nil
}

func (s *Server) CreateEnvironmentModuleVersionPin(ctx context.Context, request CreateEnvironmentModuleVersionPinRequestObject) (CreateEnvironmentModuleVersionPinResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, request.OrgId, request.Body.EnvironmentUuid, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CreateEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if environment.ProjectUuid != request.Body.ProjectUuid {
		return CreateEnvironmentModuleVersionPin400JSONResponse{N400BadRequestJSONResponse: Generate400Response("environment does not belong to project_uuid")}, nil
	}
	if err := s.checkEnvAuthorization(ctx, userID, request.OrgId, environment.ProjectId, environment.Id, PermissionModuleVersionPin); err != nil {
		return nil, err
	}
	allowDefective := request.Body.ConfirmDefectiveVersionUuid != nil && *request.Body.ConfirmDefectiveVersionUuid == request.Body.VersionUuid
	if allowDefective {
		if err := s.checkEnvAuthorization(ctx, userID, request.OrgId, environment.ProjectId, environment.Id, PermissionModuleVersionPinDefective); err != nil {
			return nil, err
		}
	}
	version, err := s.Database.GetCoreModuleVersion(ctx, nil, request.OrgId, request.Body.ModuleUuid.String(), request.Body.VersionUuid.String(), model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CreateEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.validateEnvironmentPinSnapshot(ctx, request.OrgId, *environment, *version); err != nil {
		return CreateEnvironmentModuleVersionPin409JSONResponse{N409ConflictJSONResponse: Generate409Response(err.Error())}, nil
	}
	scope := fmt.Sprintf("pin:%s:%s", request.Body.EnvironmentUuid, request.Body.ModuleUuid)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[EnvironmentModuleVersionPin](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return CreateEnvironmentModuleVersionPin409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return CreateEnvironmentModuleVersionPin201JSONResponse(response), nil
	}
	pin, err := s.Database.CreateEnvironmentModuleVersionPin(ctx, tx, request.OrgId, request.Body.ProjectUuid,
		environment.ProjectId, request.Body.EnvironmentUuid, environment.Id, request.Body.ModuleUuid,
		request.Body.VersionUuid, userID, "user", request.Body.Reason, nil, allowDefective)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return CreateEnvironmentModuleVersionPin400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return CreateEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return CreateEnvironmentModuleVersionPin409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinToAPI(*pin)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return CreateEnvironmentModuleVersionPin201JSONResponse(response), nil
}

func (s *Server) validateEnvironmentPinSnapshot(ctx context.Context, orgID string, environment model.Environment, version model.CoreModuleVersion) error {
	stateChangeOnly := true
	last, err := s.DpClient.ListLastDeploymentsWithResponse(ctx, orgID, &orchestratordp.ListLastDeploymentsParams{
		ProjectId: &environment.ProjectId, EnvId: &environment.Id, StateChangeOnly: &stateChangeOnly,
	})
	if err != nil {
		return errors.Wrap(err, "failed to inspect latest Environment Deployment")
	}
	if last.StatusCode() != http.StatusOK || last.JSON200 == nil || len(last.JSON200.Items) != 1 {
		return fmt.Errorf("Environment has no trustworthy latest state-changing Deployment")
	}
	if last.JSON200.Items[0].Status != string(ModuleVersionPinOverrideReconcileBodyOutcomeSucceeded) {
		return fmt.Errorf("latest Environment Deployment is %s; Pins require a successful reconciled state", last.JSON200.Items[0].Status)
	}
	active, err := s.DpClient.ListActiveResourceNodesWithResponse(ctx, orgID, &orchestratordp.ListActiveResourceNodesParams{
		ProjectId: &environment.ProjectId, EnvId: &environment.Id,
	})
	if err != nil {
		return errors.Wrap(err, "failed to inspect active Environment resources")
	}
	if active.StatusCode() != http.StatusOK || active.JSON200 == nil {
		return fmt.Errorf("active Environment state is unavailable")
	}
	found := false
	for _, node := range active.JSON200.Items {
		if node.ModuleId != version.ModuleSlug {
			continue
		}
		found = true
		if node.ModuleVersion != version.OpaqueVersionID {
			return fmt.Errorf("Module %s has multiple or different active versions; requested %s, observed %s", version.ModuleSlug, version.OpaqueVersionID, node.ModuleVersion)
		}
	}
	if !found {
		return fmt.Errorf("Module %s is not active in the Environment", version.ModuleSlug)
	}
	return nil
}

func (s *Server) TransitionEnvironmentModuleVersionPin(ctx context.Context, request TransitionEnvironmentModuleVersionPinRequestObject) (TransitionEnvironmentModuleVersionPinResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return TransitionEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	environment, err := s.Database.GetEnvironmentByUuid(ctx, nil, request.OrgId, pin.EnvironmentUUID, model.GetModeDefault)
	if err != nil {
		return nil, err
	}
	if err := s.checkEnvAuthorization(ctx, userID, request.OrgId, environment.ProjectId, environment.Id, PermissionModuleVersionUnpin); err != nil {
		return nil, err
	}
	if request.PinAction == TransitionEnvironmentModuleVersionPinParamsPinActionDiscard && pin.Status != moduleversions.PinOverridden {
		return TransitionEnvironmentModuleVersionPin400JSONResponse{N400BadRequestJSONResponse: Generate400Response("only an overridden Pin can be discarded permanently")}, nil
	}
	scope := fmt.Sprintf("pin:%s:%s", request.PinId, request.PinAction)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[EnvironmentModuleVersionPin](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return TransitionEnvironmentModuleVersionPin409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return TransitionEnvironmentModuleVersionPin200JSONResponse(response), nil
	}
	updated, err := s.Database.TransitionEnvironmentModuleVersionPin(ctx, tx, request.OrgId, request.PinId,
		request.Body.ExpectedResourceVersion, moduleversions.PinRemoved, string(request.PinAction), userID, "user", request.Body.Reason, nil, nil, nil)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return TransitionEnvironmentModuleVersionPin400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return TransitionEnvironmentModuleVersionPin404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return TransitionEnvironmentModuleVersionPin409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinToAPI(*updated)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return TransitionEnvironmentModuleVersionPin200JSONResponse(response), nil
}

func (s *Server) BeginEnvironmentModuleVersionPinOverride(ctx context.Context, request BeginEnvironmentModuleVersionPinOverrideRequestObject) (BeginEnvironmentModuleVersionPinOverrideResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return BeginEnvironmentModuleVersionPinOverride404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, actor, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionPinOverride); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("pin-override:%s:%s", request.PinId, request.Body.OperationId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[EnvironmentModuleVersionPin](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return BeginEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return BeginEnvironmentModuleVersionPinOverride200JSONResponse(response), nil
	}
	target, err := s.Database.GetCoreModuleVersion(ctx, tx, request.OrgId, pin.ModuleUUID.String(), request.Body.TargetVersionUuid.String(), model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return BeginEnvironmentModuleVersionPinOverride404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if target.LifecycleStatus != moduleversions.LifecycleProposed && target.LifecycleStatus != moduleversions.LifecycleDefault {
		return BeginEnvironmentModuleVersionPinOverride400JSONResponse{N400BadRequestJSONResponse: Generate400Response("Pin override target must be Proposed or Default")}, nil
	}
	updated, err := s.Database.TransitionEnvironmentModuleVersionPin(ctx, tx, request.OrgId, request.PinId,
		request.Body.ExpectedResourceVersion, moduleversions.PinOverridePending, "override_requested", actor, "addon",
		request.Body.Reason, &request.Body.OperationId, &request.Body.TargetVersionUuid, nil)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return BeginEnvironmentModuleVersionPinOverride400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return BeginEnvironmentModuleVersionPinOverride404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return BeginEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinToAPI(*updated)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return BeginEnvironmentModuleVersionPinOverride200JSONResponse(response), nil
}

func (s *Server) authoritativePinDeployment(ctx context.Context, orgID string, pin model.EnvironmentModuleVersionPin, deploymentID uuid.UUID) (*orchestratordp.Deployment, error) {
	response, err := s.DpClient.GetDeploymentWithResponse(ctx, orgID, deploymentID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve authoritative Pin Deployment")
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
		return nil, fmt.Errorf("authoritative Deployment %s is unavailable", deploymentID)
	}
	if response.JSON200.ProjectId != pin.ProjectID || response.JSON200.EnvId != pin.EnvironmentID {
		return nil, fmt.Errorf("deployment %s belongs to a different Environment", deploymentID)
	}
	return response.JSON200, nil
}

func (s *Server) ReconcileEnvironmentModuleVersionPinOverride(ctx context.Context, request ReconcileEnvironmentModuleVersionPinOverrideRequestObject) (ReconcileEnvironmentModuleVersionPinOverrideResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ReconcileEnvironmentModuleVersionPinOverride404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, actor, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionPinOverride); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("pin-reconcile:%s:%s:%s", request.PinId, request.Body.OperationId, request.Body.DeploymentId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[EnvironmentModuleVersionPin](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return ReconcileEnvironmentModuleVersionPinOverride200JSONResponse(response), nil
	}
	deployment, err := s.authoritativePinDeployment(ctx, request.OrgId, *pin, request.Body.DeploymentId)
	if err != nil {
		return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409Response(err.Error())}, nil
	}
	targetStatus, eventType := moduleversions.PinActive, "override_failed"
	switch request.Body.Outcome {
	case ModuleVersionPinOverrideReconcileBodyOutcomeSucceeded:
		if deployment.Status != string(ModuleVersionPinOverrideReconcileBodyOutcomeSucceeded) || pin.OverrideTargetVersionUUID == nil {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409Response("Deployment has not authoritatively succeeded")}, nil
		}
		environment, err := s.Database.GetEnvironmentByUuid(ctx, tx, request.OrgId, pin.EnvironmentUUID, model.GetModeDefault)
		if err != nil {
			return nil, err
		}
		target, err := s.Database.GetCoreModuleVersion(ctx, tx, request.OrgId, pin.ModuleUUID.String(), pin.OverrideTargetVersionUUID.String(), model.GetModeDefault)
		if err != nil {
			return nil, err
		}
		if err := s.validateEnvironmentPinSnapshot(ctx, request.OrgId, *environment, *target); err != nil {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409Response(err.Error())}, nil
		}
		targetStatus, eventType = moduleversions.PinOverridden, "override_succeeded"
	case ModuleVersionPinOverrideReconcileBodyOutcomeFailed:
		if deployment.Status != "failed" {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409Response("Deployment has not authoritatively failed")}, nil
		}
	case ModuleVersionPinOverrideReconcileBodyOutcomeCancelled:
		if deployment.Status != "cancelled" && deployment.Status != "terminated" {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409Response("Deployment has not been authoritatively cancelled")}, nil
		}
		eventType = "override_cancelled"
	default:
		return ReconcileEnvironmentModuleVersionPinOverride400JSONResponse{N400BadRequestJSONResponse: Generate400Response("unsupported override outcome")}, nil
	}
	updated, err := s.Database.TransitionEnvironmentModuleVersionPin(ctx, tx, request.OrgId, request.PinId,
		request.Body.ExpectedResourceVersion, targetStatus, eventType, actor, "addon", request.Body.Reason,
		&request.Body.OperationId, pin.OverrideTargetVersionUUID, &request.Body.DeploymentId)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return ReconcileEnvironmentModuleVersionPinOverride400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ReconcileEnvironmentModuleVersionPinOverride404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return ReconcileEnvironmentModuleVersionPinOverride409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinToAPI(*updated)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ReconcileEnvironmentModuleVersionPinOverride200JSONResponse(response), nil
}

func (s *Server) RestoreEnvironmentModuleVersionPinAfterRollback(ctx context.Context, request RestoreEnvironmentModuleVersionPinAfterRollbackRequestObject) (RestoreEnvironmentModuleVersionPinAfterRollbackResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := s.Database.GetEnvironmentModuleVersionPin(ctx, nil, request.OrgId, request.PinId, model.GetModeDefault)
	if err != nil {
		if notFound, ok := model.IsErrNotFound(err); ok {
			return RestoreEnvironmentModuleVersionPinAfterRollback404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	if err := s.checkPinScopeAuthorization(ctx, actor, request.OrgId, pin.EnvironmentUUID, pin.ProjectUUID, PermissionModuleVersionPinRestore); err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("pin-restore:%s:%s:%s", request.PinId, request.Body.OperationId, request.Body.DeploymentId)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[EnvironmentModuleVersionPin](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		return RestoreEnvironmentModuleVersionPinAfterRollback200JSONResponse(response), nil
	}
	if request.Body.RestoredVersionUuid != pin.VersionUUID {
		return RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse{N409ConflictJSONResponse: Generate409Response("Rollback did not restore the exact pinned Module Version")}, nil
	}
	deployment, err := s.authoritativePinDeployment(ctx, request.OrgId, *pin, request.Body.DeploymentId)
	if err != nil || deployment.Status != string(ModuleVersionPinOverrideReconcileBodyOutcomeSucceeded) || deployment.Mode != "rollback" {
		message := "authoritative rollback Deployment has not succeeded"
		if err != nil {
			message = err.Error()
		}
		return RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse{N409ConflictJSONResponse: Generate409Response(message)}, nil
	}
	environment, err := s.Database.GetEnvironmentByUuid(ctx, tx, request.OrgId, pin.EnvironmentUUID, model.GetModeDefault)
	if err != nil {
		return nil, err
	}
	version, err := s.Database.GetCoreModuleVersion(ctx, tx, request.OrgId, pin.ModuleUUID.String(), pin.VersionUUID.String(), model.GetModeDefault)
	if err != nil {
		return nil, err
	}
	if err := s.validateEnvironmentPinSnapshot(ctx, request.OrgId, *environment, *version); err != nil {
		return RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse{N409ConflictJSONResponse: Generate409Response(err.Error())}, nil
	}
	updated, err := s.Database.TransitionEnvironmentModuleVersionPin(ctx, tx, request.OrgId, request.PinId,
		request.Body.ExpectedResourceVersion, moduleversions.PinActive, "restored_after_rollback", actor, "addon",
		request.Body.Reason, &request.Body.OperationId, nil, &request.Body.DeploymentId)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return RestoreEnvironmentModuleVersionPinAfterRollback400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return RestoreEnvironmentModuleVersionPinAfterRollback404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		if conflict, ok := model.IsErrConflict(err); ok {
			return RestoreEnvironmentModuleVersionPinAfterRollback409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	}
	response := modulePinToAPI(*updated)
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, actor, response); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return RestoreEnvironmentModuleVersionPinAfterRollback200JSONResponse(response), nil
}

type modulePinBulkDecision struct {
	item        ModuleVersionPinBulkPreviewItem
	environment model.Environment
	version     *model.CoreModuleVersion
	pin         *model.EnvironmentModuleVersionPin
}

func modulePinPreviewHash(action ModuleVersionPinBulkAction, moduleUUID uuid.UUID, items []ModuleVersionPinBulkPreviewItem) string {
	encoded, err := json.Marshal(struct {
		Action     ModuleVersionPinBulkAction
		ModuleUUID uuid.UUID
		Items      []ModuleVersionPinBulkPreviewItem
	}{Action: action, ModuleUUID: moduleUUID, Items: items})
	if err != nil {
		panic(fmt.Sprintf("encode typed Pin bulk preview: %v", err))
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (s *Server) currentEnvironmentModuleVersion(ctx context.Context, tx model.Tx, orgID string, environment model.Environment, module model.ModuleCatalogue) (*model.CoreModuleVersion, error) {
	stateChangeOnly := true
	last, err := s.DpClient.ListLastDeploymentsWithResponse(ctx, orgID, &orchestratordp.ListLastDeploymentsParams{
		ProjectId: &environment.ProjectId, EnvId: &environment.Id, StateChangeOnly: &stateChangeOnly,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to inspect latest Environment Deployment")
	}
	if last.StatusCode() != http.StatusOK || last.JSON200 == nil || len(last.JSON200.Items) != 1 || last.JSON200.Items[0].Status != string(ModuleVersionPinOverrideReconcileBodyOutcomeSucceeded) {
		return nil, fmt.Errorf("Environment has no trustworthy latest successful state-changing Deployment")
	}
	active, err := s.DpClient.ListActiveResourceNodesWithResponse(ctx, orgID, &orchestratordp.ListActiveResourceNodesParams{
		ProjectId: &environment.ProjectId, EnvId: &environment.Id,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to inspect active Environment resources")
	}
	if active.StatusCode() != http.StatusOK || active.JSON200 == nil {
		return nil, fmt.Errorf("active Environment state is unavailable")
	}
	versionID := ""
	for _, node := range active.JSON200.Items {
		if node.ModuleId != module.Slug {
			continue
		}
		if versionID != "" && versionID != node.ModuleVersion {
			return nil, fmt.Errorf("Module %s has multiple active Versions", module.Slug)
		}
		versionID = node.ModuleVersion
	}
	if versionID == "" {
		return nil, fmt.Errorf("Module %s is not active in the Environment", module.Slug)
	}
	return s.Database.GetCoreModuleVersion(ctx, tx, orgID, module.UUID.String(), versionID, model.GetModeDefault)
}

func (s *Server) previewModulePinBulk(ctx context.Context, tx model.Tx, userID uuid.UUID, orgID string, body ModuleVersionPinBulkPreviewBody) ([]modulePinBulkDecision, error) {
	module, err := s.Database.GetModuleCatalogue(ctx, tx, orgID, body.ModuleUuid.String(), model.GetModeDefault)
	if err != nil {
		return nil, err
	}
	confirmDefective := make(map[uuid.UUID]struct{})
	if body.ConfirmDefectiveVersionUuids != nil {
		for _, versionID := range *body.ConfirmDefectiveVersionUuids {
			confirmDefective[versionID] = struct{}{}
		}
	}
	environmentIDs := slices.Clone(body.EnvironmentUuids)
	slices.SortFunc(environmentIDs, func(left, right uuid.UUID) int { return strings.Compare(left.String(), right.String()) })
	decisions := make([]modulePinBulkDecision, 0, len(environmentIDs))
	for _, environmentUUID := range environmentIDs {
		decision := modulePinBulkDecision{item: ModuleVersionPinBulkPreviewItem{EnvironmentUuid: environmentUUID}}
		environment, err := s.Database.GetEnvironmentByUuid(ctx, tx, orgID, environmentUUID, model.GetModeDefault)
		if err != nil {
			problem := "Environment not found"
			decision.item.Problem = &problem
			decisions = append(decisions, decision)
			continue
		}
		permission := PermissionModuleVersionPin
		if body.Action != ModuleVersionPinBulkActionPin {
			permission = PermissionModuleVersionUnpin
		}
		if err := s.checkPinScopeAuthorization(ctx, userID, orgID, environment.Uuid, environment.ProjectUuid, permission); err != nil {
			problem := "not authorised for this Environment"
			decision.item.Problem = &problem
			decisions = append(decisions, decision)
			continue
		}
		decision.environment = *environment
		decision.item.ProjectUuid, decision.item.ProjectId = environment.ProjectUuid, environment.ProjectId
		decision.item.EnvironmentId, decision.item.EnvironmentType = environment.Id, &environment.EnvTypeId
		decision.item.Production = strings.Contains(strings.ToLower(environment.EnvTypeId), "prod") || strings.EqualFold(environment.Labels["tier"], "production")
		pins, err := s.Database.ListEnvironmentModuleVersionPins(ctx, tx, orgID, &environment.Uuid, &module.UUID, true)
		if err != nil {
			return nil, err
		}
		for index := range pins {
			if pins[index].Status != moduleversions.PinRemoved {
				decision.pin = &pins[index]
				break
			}
		}
		switch body.Action {
		case ModuleVersionPinBulkActionPin:
			version, err := s.currentEnvironmentModuleVersion(ctx, tx, orgID, *environment, *module)
			if err != nil {
				problem := err.Error()
				decision.item.Problem = &problem
				break
			}
			decision.version, decision.item.VersionUuid = version, &version.UUID
			if version.LifecycleStatus == moduleversions.LifecycleDefective {
				if _, confirmed := confirmDefective[version.UUID]; !confirmed {
					problem := "Defective Version requires exact confirmation"
					decision.item.Problem = &problem
					break
				}
				if err := s.checkPinScopeAuthorization(ctx, userID, orgID, environment.Uuid, environment.ProjectUuid, PermissionModuleVersionPinDefective); err != nil {
					problem := "Defective Version requires module.version.pin-defective"
					decision.item.Problem = &problem
					break
				}
			}
			if decision.pin != nil {
				decision.item.PinId, decision.item.PinResourceVersion = &decision.pin.ID, &decision.pin.ResourceVersion
				if decision.pin.Status == moduleversions.PinActive && decision.pin.VersionUUID == version.UUID {
					match := true
					decision.item.IdempotentMatch = &match
					decision.item.Eligible = true
				} else {
					problem := fmt.Sprintf("existing Pin %s is %s for another Version or operation", decision.pin.ID, decision.pin.Status)
					decision.item.Problem = &problem
				}
			} else {
				decision.item.Eligible = true
			}
		case ModuleVersionPinBulkActionUnpin:
			if decision.pin == nil || decision.pin.Status != moduleversions.PinActive {
				problem := "no active Pin exists"
				decision.item.Problem = &problem
				break
			}
			decision.item.PinId, decision.item.PinResourceVersion = &decision.pin.ID, &decision.pin.ResourceVersion
			decision.item.VersionUuid, decision.item.Eligible = &decision.pin.VersionUUID, true
		case ModuleVersionPinBulkActionDiscard:
			if decision.pin == nil || decision.pin.Status != moduleversions.PinOverridden {
				problem := "no overridden Pin exists"
				decision.item.Problem = &problem
				break
			}
			decision.item.PinId, decision.item.PinResourceVersion = &decision.pin.ID, &decision.pin.ResourceVersion
			decision.item.VersionUuid, decision.item.Eligible = &decision.pin.VersionUUID, true
		default:
			return nil, model.NewErrBadRequest("unsupported Pin bulk action")
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func pinBulkPreviewResponse(body ModuleVersionPinBulkPreviewBody, decisions []modulePinBulkDecision) ModuleVersionPinBulkPreview {
	items := make([]ModuleVersionPinBulkPreviewItem, 0, len(decisions))
	eligible := true
	for _, decision := range decisions {
		items = append(items, decision.item)
		eligible = eligible && decision.item.Eligible
	}
	return ModuleVersionPinBulkPreview{
		OperationId: uuid.New(), Action: body.Action, ModuleUuid: body.ModuleUuid, Eligible: eligible,
		Fingerprint: modulePinPreviewHash(body.Action, body.ModuleUuid, items), Items: items,
	}
}

func (s *Server) PreviewEnvironmentModuleVersionPinBulkOperation(ctx context.Context, request PreviewEnvironmentModuleVersionPinBulkOperationRequestObject) (PreviewEnvironmentModuleVersionPinBulkOperationResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	decisions, err := s.previewModulePinBulk(ctx, nil, userID, request.OrgId, *request.Body)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return PreviewEnvironmentModuleVersionPinBulkOperation400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return PreviewEnvironmentModuleVersionPinBulkOperation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	return PreviewEnvironmentModuleVersionPinBulkOperation200JSONResponse(pinBulkPreviewResponse(*request.Body, decisions)), nil
}

func (s *Server) ExecuteEnvironmentModuleVersionPinBulkOperation(ctx context.Context, request ExecuteEnvironmentModuleVersionPinBulkOperationRequestObject) (ExecuteEnvironmentModuleVersionPinBulkOperationResponseObject, error) {
	userID, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("pin-bulk:%s:%s", request.Body.Action, request.Body.ModuleUuid)
	fingerprint := commandIdentity(scope, request.Body)
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if response, found, err := readModuleCommand[ModuleVersionPinBulkResult](ctx, s.Database, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint); err != nil {
		if conflict, ok := model.IsErrConflict(err); ok {
			return ExecuteEnvironmentModuleVersionPinBulkOperation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
		}
		return nil, err
	} else if found {
		permission := PermissionModuleVersionPin
		if request.Body.Action != ModuleVersionPinBulkActionPin {
			permission = PermissionModuleVersionUnpin
		}
		for _, pin := range response.Pins {
			if err := s.checkPinScopeAuthorization(ctx, userID, request.OrgId, pin.EnvironmentUuid, pin.ProjectUuid, permission); err != nil {
				return nil, err
			}
			if request.Body.Action == ModuleVersionPinBulkActionPin && request.Body.ConfirmDefectiveVersionUuids != nil &&
				slices.Contains(*request.Body.ConfirmDefectiveVersionUuids, pin.VersionUuid) {
				if err := s.checkPinScopeAuthorization(ctx, userID, request.OrgId, pin.EnvironmentUuid, pin.ProjectUuid, PermissionModuleVersionPinDefective); err != nil {
					return nil, err
				}
			}
		}
		return ExecuteEnvironmentModuleVersionPinBulkOperation200JSONResponse(response), nil
	}
	previewBody := ModuleVersionPinBulkPreviewBody{
		Action: request.Body.Action, ModuleUuid: request.Body.ModuleUuid, EnvironmentUuids: request.Body.EnvironmentUuids,
		ConfirmDefectiveVersionUuids: request.Body.ConfirmDefectiveVersionUuids,
	}
	decisions, err := s.previewModulePinBulk(ctx, tx, userID, request.OrgId, previewBody)
	if err != nil {
		if badRequest, ok := model.IsErrBadRequest(err); ok {
			return ExecuteEnvironmentModuleVersionPinBulkOperation400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
		}
		if notFound, ok := model.IsErrNotFound(err); ok {
			return ExecuteEnvironmentModuleVersionPinBulkOperation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
		}
		return nil, err
	}
	preview := pinBulkPreviewResponse(previewBody, decisions)
	if preview.Fingerprint != request.Body.PreviewFingerprint {
		return ExecuteEnvironmentModuleVersionPinBulkOperation409JSONResponse{N409ConflictJSONResponse: Generate409Response("Pin bulk preview is stale; review the exact Environment set again")}, nil
	}
	if !preview.Eligible {
		details := map[string]interface{}{"preview": preview}
		response := Generate409Response("Pin bulk operation is not eligible for every Environment")
		response.Details = &details
		return ExecuteEnvironmentModuleVersionPinBulkOperation409JSONResponse{N409ConflictJSONResponse: response}, nil
	}
	operationID := uuid.New()
	result := ModuleVersionPinBulkResult{OperationId: operationID, Action: request.Body.Action, Pins: make([]EnvironmentModuleVersionPin, 0, len(decisions))}
	confirmDefective := make(map[uuid.UUID]struct{})
	if request.Body.ConfirmDefectiveVersionUuids != nil {
		for _, versionID := range *request.Body.ConfirmDefectiveVersionUuids {
			confirmDefective[versionID] = struct{}{}
		}
	}
	for _, decision := range decisions {
		var pin *model.EnvironmentModuleVersionPin
		switch request.Body.Action {
		case ModuleVersionPinBulkActionPin:
			if decision.item.IdempotentMatch != nil && *decision.item.IdempotentMatch {
				pin, err = s.Database.GetEnvironmentModuleVersionPin(ctx, tx, request.OrgId, decision.pin.ID, model.GetModeForUpdate)
				if err == nil && (pin.Status != moduleversions.PinActive || pin.VersionUUID != decision.version.UUID || pin.ResourceVersion != decision.pin.ResourceVersion) {
					err = model.NewErrConflict("idempotently matched Pin changed after preview")
				}
			} else {
				_, allowDefective := confirmDefective[decision.version.UUID]
				pin, err = s.Database.CreateEnvironmentModuleVersionPin(ctx, tx, request.OrgId,
					decision.environment.ProjectUuid, decision.environment.ProjectId, decision.environment.Uuid,
					decision.environment.Id, request.Body.ModuleUuid, decision.version.UUID, userID, "user",
					request.Body.Reason, &operationID, allowDefective)
			}
		case ModuleVersionPinBulkActionUnpin, ModuleVersionPinBulkActionDiscard:
			eventType := "unpinned"
			if request.Body.Action == ModuleVersionPinBulkActionDiscard {
				eventType = "discarded"
			}
			pin, err = s.Database.TransitionEnvironmentModuleVersionPin(ctx, tx, request.OrgId, decision.pin.ID,
				decision.pin.ResourceVersion, moduleversions.PinRemoved, eventType, userID, "user", request.Body.Reason,
				nil, nil, nil)
		}
		if err != nil {
			if badRequest, ok := model.IsErrBadRequest(err); ok {
				return ExecuteEnvironmentModuleVersionPinBulkOperation400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(badRequest)}, nil
			}
			if notFound, ok := model.IsErrNotFound(err); ok {
				return ExecuteEnvironmentModuleVersionPinBulkOperation404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(notFound)}, nil
			}
			if conflict, ok := model.IsErrConflict(err); ok {
				return ExecuteEnvironmentModuleVersionPinBulkOperation409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
			}
			return nil, err
		}
		result.Pins = append(result.Pins, modulePinToAPI(*pin))
	}
	if err := s.Database.StoreModuleCoreCommand(ctx, tx, request.OrgId, scope, request.Params.IdempotencyKey, fingerprint, userID, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ExecuteEnvironmentModuleVersionPinBulkOperation200JSONResponse(result), nil
}

func readModuleCommand[T any](ctx context.Context, database model.Databaser, tx model.Tx, orgID, scope, key, fingerprint string) (T, bool, error) {
	var zero T
	storedFingerprint, response, found, err := database.GetModuleCoreCommand(ctx, tx, orgID, scope, key)
	if err != nil || !found {
		return zero, false, err
	}
	if storedFingerprint != fingerprint {
		return zero, false, model.NewErrConflict("idempotency key was used for a different command")
	}
	if err := json.Unmarshal(response, &zero); err != nil {
		return zero, false, errors.Wrap(err, "failed to decode idempotent module command response")
	}
	return zero, true, nil
}
