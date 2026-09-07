package api

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/htelemetry"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/errcodes"
)

const (
	getModuleCataloguePaginationSize = 200
)

type resourceKind struct {
	Type  string
	Class string
	Id    opt.Opt[string]
}

func resolvedPinVersionUUID(pin model.EnvironmentModuleVersionPin, operationID *uuid.UUID) uuid.UUID {
	if pin.Status == moduleversions.PinOverridePending && operationID != nil && pin.OverrideOperationID != nil &&
		*operationID == *pin.OverrideOperationID && pin.OverrideTargetVersionUUID != nil {
		return *pin.OverrideTargetVersionUUID
	}
	return pin.VersionUUID
}

func permitsArchivedModuleVersion(status moduleversions.CatalogueStatus, coordinate string, activeVersions map[string]struct{}, rollback bool) bool {
	if status != moduleversions.CatalogueArchived {
		return true
	}
	if rollback {
		return true
	}
	_, carriedForward := activeVersions[coordinate]
	return carriedForward
}

func permitsRestrictedModuleVersion(status string, activePin, rollback, exactConfirmation bool) bool {
	switch status {
	case string(moduleversions.LifecycleDeprecated):
		return activePin || rollback
	case string(moduleversions.LifecycleDefective):
		return exactConfirmation && (activePin || rollback)
	default:
		return true
	}
}

func calculateSpecificity(bits ...opt.Opt[string]) int {
	n := 0
	for _, bit := range bits {
		n <<= 1
		if bit.IsSet() {
			n++
		}
	}
	return n
}

// getAllModuleRulesAndDefinitionIds returns all the rules that apply to the environment along with the definition ids for those rules.
func (s *Server) getAllModuleRulesAndDefinitionIds(ctx context.Context, orgId string, projectId string, envTypeId string, envId string, tx model.Tx, activeVersions map[string]struct{}) ([]model.DefinitionRule, map[string]bool, map[string]string, error) {
	allRules := make([]model.DefinitionRule, 0)
	span, ctx := htelemetry.StartSpanFromContext(ctx, "gen-catalogue-list-rules")
	defer span.Finish()

	maxSpecificityPerResourceKind := make(map[resourceKind]int)
	moduleStatuses := make(map[string]moduleversions.CatalogueStatus)
	archivedVersions := make(map[string]string)

	for pageToken, pageNum := "", 1; pageNum == 1 || pageToken != ""; pageNum++ {
		if page, nextPageToken, err := s.Database.ListModuleRules(ctx, tx, orgId, pageToken, getModuleCataloguePaginationSize, model.ListModuleRulesParams{
			EffectiveInProjectId: &projectId,
			EffectiveInEnvId:     &envId,
			EffectiveInEnvTypeId: &envTypeId,
		}); err != nil {
			span.Finish(htelemetry.WithError(err))
			return nil, nil, nil, errors.Wrapf(err, "failed to list module rules on page %d (#allRules=%d)", pageNum, len(allRules))
		} else {
			for _, rule := range page {
				status, known := moduleStatuses[rule.DefinitionId]
				if !known {
					module, getErr := s.Database.GetModuleCatalogue(ctx, tx, orgId, rule.DefinitionId, model.GetModeDefault)
					if getErr != nil {
						return nil, nil, nil, errors.Wrap(getErr, "failed to resolve Module catalogue state for rule")
					}
					status = module.Status
					moduleStatuses[rule.DefinitionId] = status
				}
				if status == moduleversions.CatalogueArchived {
					version, err := archivedCarryForwardVersion(rule.DefinitionId, activeVersions)
					if err != nil {
						return nil, nil, nil, err
					}
					if version == "" {
						continue
					}
					archivedVersions[rule.DefinitionId] = version
				}
				kind := resourceKind{Type: rule.ResourceType, Class: rule.ResourceClass, Id: rule.ResourceId}
				specificity := calculateSpecificity(rule.EnvId, rule.ProjectId, rule.EnvTypeId)
				if m, ok := maxSpecificityPerResourceKind[kind]; !ok || specificity > m {
					maxSpecificityPerResourceKind[kind] = specificity
					allRules = append(allRules, rule)
				}
			}
			pageToken = nextPageToken
		}
	}

	// NOTE: we do the specificity filtering outside the database query because it is more readable and saves on a lot of DB connection memory.
	// Historically this was one of the most expensive queries in platform-orchestrator 1, and we have a chance to do this slightly differently.

	seenDefinitions := make(map[string]bool)
	allRules = slices.Collect(
		// filter the list so that we're only picking the max specificity entries for each resource kind
		func(yield func(rule model.DefinitionRule) bool) {
			for _, rule := range allRules {
				kind := resourceKind{Type: rule.ResourceType, Class: rule.ResourceClass, Id: rule.ResourceId}
				specificity := calculateSpecificity(rule.EnvId, rule.ProjectId, rule.EnvTypeId)
				if specificity == maxSpecificityPerResourceKind[kind] {
					seenDefinitions[rule.DefinitionId] = true
					yield(rule)
				}
			}
		},
	)
	return allRules, seenDefinitions, archivedVersions, nil
}

func archivedCarryForwardVersion(moduleID string, activeVersions map[string]struct{}) (string, error) {
	version := ""
	for coordinate := range activeVersions {
		id, candidate, found := strings.Cut(coordinate, "@")
		if !found || id != moduleID || candidate == "" {
			continue
		}
		if version != "" && version != candidate {
			return "", model.NewErrConflict(fmt.Sprintf("archived module %s has multiple effective versions; use retained Deployment rollback to preserve their exact assignments", moduleID))
		}
		version = candidate
	}
	return version, nil
}

func (s *Server) getAllDefinitionsForRules(ctx context.Context, orgId string, definitionIds []string, tx model.Tx) ([]model.ModuleDefinitionVersion, map[string]bool, error) {
	allDefinitions := make([]model.ModuleDefinitionVersion, 0, len(definitionIds))
	seenProviders := make(map[string]bool)
	span, ctx := htelemetry.StartSpanFromContext(ctx, "gen-catalogue-list-defs")
	defer span.Finish()

	for pageToken, pageNum := "", 1; pageNum == 1 || pageToken != ""; pageNum++ {
		if page, nextPageToken, err := s.Database.ListModuleDefinitions(ctx, tx, orgId, pageToken, getModuleCataloguePaginationSize, model.ListModuleDefinitionsParams{
			ByDefinitionIds: definitionIds,
		}); err != nil {
			span.Finish(htelemetry.WithError(err))
			return nil, nil, errors.Wrapf(err, "failed to list modules on page %d (#allDefinitions=%d)", pageNum, len(allDefinitions))
		} else {
			allDefinitions = append(allDefinitions, page...)
			for _, def := range page {
				for v := range maps.Values(def.ProviderMapping) {
					seenProviders[v] = true
				}
			}
			pageToken = nextPageToken
		}
	}
	// Very important to ensure we get all the definitions we expected to get!
	if len(allDefinitions) != len(definitionIds) {
		missingDefinitions := make([]string, 0)
		for _, definitionId := range definitionIds {
			if !slices.ContainsFunc(allDefinitions, func(version model.ModuleDefinitionVersion) bool {
				return version.DefinitionId == definitionId
			}) {
				missingDefinitions = append(missingDefinitions, definitionId)
			}
		}
		return nil, nil, errors.New("failed to retrieve some modules: " + strings.Join(missingDefinitions, ", "))
	}
	return allDefinitions, seenProviders, nil
}

func (s *Server) getPinnedDefinitions(ctx context.Context, orgId string, tx model.Tx, ids []string, versionIds []string) ([]model.ModuleDefinitionVersion, map[string]bool, error) {
	span, ctx := htelemetry.StartSpanFromContext(ctx, "gen-catalogue-list-defs-pinned")
	defer span.Finish()

	defs, err := s.Database.BulkGetModuleDefinitionVersions(ctx, tx, orgId, ids, versionIds)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to get pinned modules")
	}
	seenProviders := make(map[string]bool)
	for _, def := range defs {
		for v := range maps.Values(def.ProviderMapping) {
			seenProviders[v] = true
		}
	}
	return defs, seenProviders, nil
}

func (s *Server) GenerateInternalModuleCatalogue(ctx context.Context, request GenerateInternalModuleCatalogueRequestObject) (GenerateInternalModuleCatalogueResponseObject, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx)
	if request.Body == nil {
		request.Body = &InternalModuleCatalogueGenerateBody{}
	}
	activeModuleVersions := make(map[string]struct{})
	if request.Params.XStellwerkActiveModuleVersions != nil {
		for _, coordinate := range *request.Params.XStellwerkActiveModuleVersions {
			activeModuleVersions[coordinate] = struct{}{}
		}
	}

	var allRules []model.DefinitionRule
	var allDefinitions []model.ModuleDefinitionVersion
	var allProviders []model.ModuleProvider
	pinAuthorizedVersions := make(map[uuid.UUID]bool)
	if tx, err := s.Database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		env, err := s.Database.GetEnvironment(ctx, tx, request.OrgId, request.ProjectId, request.EnvId, model.GetModeDefault)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return GenerateInternalModuleCatalogue404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get environment")
		}
		var corePins []model.EnvironmentModuleVersionPin
		if !ref.DerefOr(request.Params.XStellwerkModuleOperationPlanning, false) {
			corePins, err = s.Database.ListEnvironmentModuleVersionPins(ctx, tx, request.OrgId, &env.Uuid, nil, false)
			if err != nil {
				return nil, errors.Wrap(err, "failed to resolve Environment Module Version Pins")
			}
		}
		type resolvedCorePin struct {
			moduleID string
			version  string
		}
		resolvedPins := make([]resolvedCorePin, 0, len(corePins))
		for _, pin := range corePins {
			if pin.Status != moduleversions.PinActive && pin.Status != moduleversions.PinOverridePending {
				continue
			}
			versionUUID := resolvedPinVersionUUID(pin, request.Params.XStellwerkModuleOperationId)
			pinAuthorizedVersions[versionUUID] = true
			version, err := s.Database.GetCoreModuleVersion(ctx, tx, request.OrgId, pin.ModuleUUID.String(), versionUUID.String(), model.GetModeDefault)
			if err != nil {
				return nil, errors.Wrap(err, "failed to resolve pinned immutable Module Version")
			}
			resolvedPins = append(resolvedPins, resolvedCorePin{moduleID: version.ModuleSlug, version: version.OpaqueVersionID})
		}

		var seenProviders map[string]bool
		definitionVersion := make(map[string]bool)

		if !request.Body.AreRulesIgnored {
			var seenDefinitions map[string]bool
			var archivedVersions map[string]string
			allRules, seenDefinitions, archivedVersions, err = s.getAllModuleRulesAndDefinitionIds(ctx, request.OrgId, env.ProjectId, env.EnvTypeId, env.Id, tx, activeModuleVersions)
			if err != nil {
				if conflict, ok := model.IsErrConflict(err); ok {
					return GenerateInternalModuleCatalogue409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(conflict)}, nil
				}
				return nil, err
			}
			// Archived Modules retain rules only for their exact effective Version.
			// Loading today's Default here could silently adopt a different Version.
			for _, moduleID := range slices.Sorted(maps.Keys(archivedVersions)) {
				delete(seenDefinitions, moduleID)
				request.Body.PinnedModuleVersions = append(request.Body.PinnedModuleVersions, moduleID+"@"+archivedVersions[moduleID])
			}
			seenProviders = make(map[string]bool)
			if len(seenDefinitions) > 0 {
				allDefinitions, seenProviders, err = s.getAllDefinitionsForRules(ctx, request.OrgId, slices.Sorted(maps.Keys(seenDefinitions)), tx)
				if err != nil {
					return nil, err
				}
			}
			// We need to make sure we don't pull extra definitions if we've already retrieved these
			for _, rule := range allDefinitions {
				definitionVersion[rule.DefinitionId+"@"+rule.VersionId] = true
			}
		} else {
			seenProviders = make(map[string]bool, len(request.Body.PinnedProviders))
		}
		for _, pin := range resolvedPins {
			for _, requested := range request.Body.PinnedModuleVersions {
				parts := strings.SplitN(requested, "@", 2)
				if len(parts) == 2 && parts[0] == pin.moduleID && parts[1] != pin.version {
					return GenerateInternalModuleCatalogue409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf(
						"active Environment Pin requires %s@%s, requested %s", pin.moduleID, pin.version, requested,
					))}, nil
				}
			}
			allDefinitions = slices.DeleteFunc(allDefinitions, func(definition model.ModuleDefinitionVersion) bool {
				return definition.DefinitionId == pin.moduleID && definition.VersionId != pin.version
			})
			request.Body.PinnedModuleVersions = append(request.Body.PinnedModuleVersions, pin.moduleID+"@"+pin.version)
		}

		// seed the requested providers map with the pinned providers
		for _, prov := range request.Body.PinnedProviders {
			seenProviders[prov] = true
		}

		// Work out which of the requested pinned version we don't need to pull because we have them already
		pinnedDefinitionIds := make([]string, 0)
		pinnedVersionIds := make([]string, 0)
		for _, version := range request.Body.PinnedModuleVersions {
			if !definitionVersion[version] {
				parts := strings.Split(version, "@")
				if len(parts) != 2 {
					return nil, errors.Errorf("invalid pinned version '%s'", version)
				}
				pinnedDefinitionIds = append(pinnedDefinitionIds, parts[0])
				pinnedVersionIds = append(pinnedVersionIds, parts[1])
			}
		}

		if len(pinnedDefinitionIds) > 0 {
			pinnedDefinitions, pinnedDefinitionProviders, err := s.getPinnedDefinitions(ctx, request.OrgId, tx, pinnedDefinitionIds, pinnedVersionIds)
			if err != nil {
				return nil, err
			}
			allDefinitions = append(allDefinitions, pinnedDefinitions...)
			maps.Copy(seenProviders, pinnedDefinitionProviders)
		}

		allProviders = make([]model.ModuleProvider, 0, len(seenProviders))
		if len(seenProviders) > 0 {
			span, ctx := htelemetry.StartSpanFromContext(ctx, "gen-catalogue-list-provs")
			defer span.Finish()

			for pageToken, pageNum := "", 1; pageNum == 1 || pageToken != ""; pageNum++ {
				if page, nextPageToken, err := s.Database.ListModuleProviders(ctx, tx, request.OrgId, pageToken, getModuleCataloguePaginationSize, model.ListModuleProvidersParams{
					ByProviderIds: slices.Sorted(maps.Keys(seenProviders)),
				}); err != nil {
					span.Finish(htelemetry.WithError(err))
					return nil, errors.Wrapf(err, "failed to list module providers on page %d (#allProviders=%d)", pageNum, len(allProviders))
				} else {
					allProviders = append(allProviders, page...)
					pageToken = nextPageToken
				}
			}
			span.Finish()
		}
		// Very important that we verify that we got all the providers back that we expected!
		if len(allProviders) != len(seenProviders) {
			missingProviders := make([]string, 0)
			for provTypeId := range seenProviders {
				parts := strings.Split(provTypeId, ".")
				if len(parts) != 2 {
					return nil, errors.Errorf("invalid provider type id '%s'", provTypeId)
				}
				if !slices.ContainsFunc(allProviders, func(prov model.ModuleProvider) bool {
					return prov.ProviderType == parts[0] && prov.Id == parts[1]
				}) {
					missingProviders = append(missingProviders, provTypeId)
				}
			}
			slices.Sort(missingProviders)
			return GenerateInternalModuleCatalogue409JSONResponse{N409ConflictJSONResponse{
				Error:   string(errcodes.PinnedModuleMissingProvider),
				Message: "some providers were missing for requested modules",
				Details: &map[string]interface{}{
					"missing_providers": missingProviders,
				},
			}}, nil
		}
	}

	out := InternalModuleCatalogue{
		Modules:   make([]InternalModuleCatalogueModule, 0, len(allDefinitions)),
		Providers: make([]ModuleProvider, 0, len(allProviders)),
	}
	restrictedConfirmations := make(map[uuid.UUID]bool)
	if request.Params.XStellwerkRestrictedVersionConfirmations != nil {
		for _, versionUUID := range *request.Params.XStellwerkRestrictedVersionConfirmations {
			restrictedConfirmations[versionUUID] = true
		}
	}
	for _, definition := range allDefinitions {
		module, err := s.Database.GetModuleCatalogue(ctx, nil, request.OrgId, definition.DefinitionId, model.GetModeDefault)
		if err != nil {
			return nil, errors.Wrap(err, "failed to validate Module catalogue state")
		}
		coordinate := definition.DefinitionId + "@" + definition.VersionId
		if !permitsArchivedModuleVersion(module.Status, coordinate, activeModuleVersions, ref.DerefOr(request.Params.XStellwerkRollback, false)) {
			return GenerateInternalModuleCatalogue409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf(
				"module %s is archived and rejects new Environment adoption", definition.DefinitionId,
			))}, nil
		}
		if !permitsRestrictedModuleVersion(definition.SemanticStatus, pinAuthorizedVersions[definition.VersionUUID],
			ref.DerefOr(request.Params.XStellwerkRollback, false), restrictedConfirmations[definition.VersionUUID]) {
			return GenerateInternalModuleCatalogue409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf("module %s@%s is %s and cannot be used for a normal deployment", definition.DefinitionId, definition.VersionId, definition.SemanticStatus))}, nil
		}
	}

	for _, prov := range allProviders {
		out.Providers = append(out.Providers, apiMpFromDbMp(prov))
	}

	rulesByDefinitionId := make(map[string][]InternalModuleCatalogueModuleRule, len(allRules))
	for _, rule := range allRules {
		rules := rulesByDefinitionId[rule.DefinitionId]
		rules = append(rules, InternalModuleCatalogueModuleRule{
			RuleId:        rule.Id,
			ResourceClass: rule.ResourceClass,
			ResourceId:    rule.ResourceId.Ref(),
			ProjectId:     rule.ProjectId.Ref(),
			EnvId:         rule.EnvId.Ref(),
			EnvTypeId:     rule.EnvTypeId.Ref(),
		})
		rulesByDefinitionId[rule.DefinitionId] = rules
	}

	for _, def := range allDefinitions {
		deps := make(map[string]ModuleDependencyManifest)
		for alias, dependency := range def.Dependencies {
			deps[alias] = ModuleDependencyManifest{
				Type:   dependency.Type,
				Class:  dependency.Class.Ref(),
				Id:     dependency.Id.Ref(),
				Params: dependency.Params,
			}
		}

		coprovisioned := make([]ModuleCoProvisionManifest, 0, len(def.CoProvisioned))
		for _, c := range def.CoProvisioned {
			coprovisioned = append(coprovisioned, ModuleCoProvisionManifest{
				Type:                      c.Type,
				Id:                        c.Id.Ref(),
				Class:                     c.Class.Ref(),
				Params:                    c.Params,
				IsDependentOnCurrent:      c.IsDependentOnCurrent,
				CopyDependentsFromCurrent: c.CopyDependentsFromCurrent,
			})
		}

		moduleParams := make(map[string]ModuleParamItem, len(def.ModuleParams))
		for k, p := range def.ModuleParams {
			moduleParams[k] = ModuleParamItem{
				IsOptional: p.IsOptional,
				Type:       ModuleParamItemType(p.Type),
			}
		}

		// Once we've used the rules in the first copy of the definition, we can drop them and avoid any pinned versions
		// for the same definition
		rules, ok := rulesByDefinitionId[def.DefinitionId]
		if ok {
			delete(rulesByDefinitionId, def.DefinitionId)
		}

		out.Modules = append(out.Modules, InternalModuleCatalogueModule{
			ArtifactDigest:      def.ArtifactDigest,
			CreatedAt:           def.CreatedAt,
			Dependencies:        deps,
			Coprovisioned:       coprovisioned,
			Description:         def.Description.Ref(),
			Id:                  def.DefinitionId,
			ModuleInputs:        def.ModuleInputs,
			ModuleParams:        moduleParams,
			ModuleSource:        def.ModuleSource,
			ModuleSourceCode:    def.ModuleSourceCode.Ref(),
			OrgId:               def.OrgId,
			ProviderMapping:     def.ProviderMapping,
			ResourceType:        def.ResourceType,
			Rules:               rules,
			UpdatedAt:           def.UpdatedAt,
			VersionId:           def.VersionId,
			SemanticStatus:      ModuleVersionSemanticStatus(def.SemanticStatus),
			ModuleUuid:          def.ModuleUUID,
			VersionUuid:         def.VersionUUID,
			SemanticVersion:     def.SemanticVersion,
			MigrationGeneration: InternalModuleCatalogueModuleMigrationGeneration(def.MigrationGeneration),
			VerificationStatus:  ModuleVerificationStatus(def.VerificationStatus),
		})
	}

	return GenerateInternalModuleCatalogue200JSONResponse(out), nil
}
