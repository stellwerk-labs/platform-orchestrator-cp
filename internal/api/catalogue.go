package api

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strings"

	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/htelemetry"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/errcodes"
)

const (
	getModuleCataloguePaginationSize = 200
)

type resourceKind struct {
	Type  string
	Class string
	Id    opt.Opt[string]
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
func (s *Server) getAllModuleRulesAndDefinitionIds(ctx context.Context, orgId string, projectId string, envTypeId string, envId string, tx model.Tx) ([]model.DefinitionRule, map[string]bool, error) {
	allRules := make([]model.DefinitionRule, 0)
	span, ctx := htelemetry.StartSpanFromContext(ctx, "gen-catalogue-list-rules")
	defer span.Finish()

	maxSpecificityPerResourceKind := make(map[resourceKind]int)

	for pageToken, pageNum := "", 1; pageNum == 1 || pageToken != ""; pageNum++ {
		if page, nextPageToken, err := s.Database.ListModuleRules(ctx, tx, orgId, pageToken, getModuleCataloguePaginationSize, model.ListModuleRulesParams{
			EffectiveInProjectId: &projectId,
			EffectiveInEnvId:     &envId,
			EffectiveInEnvTypeId: &envTypeId,
		}); err != nil {
			span.Finish(htelemetry.WithError(err))
			return nil, nil, errors.Wrapf(err, "failed to list module rules on page %d (#allRules=%d)", pageNum, len(allRules))
		} else {
			for _, rule := range page {
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
	return allRules, seenDefinitions, nil
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

	var allRules []model.DefinitionRule
	var allDefinitions []model.ModuleDefinitionVersion
	var allProviders []model.ModuleProvider
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

		var seenProviders map[string]bool
		definitionVersion := make(map[string]bool)

		if !request.Body.AreRulesIgnored {
			var seenDefinitions map[string]bool
			allRules, seenDefinitions, err = s.getAllModuleRulesAndDefinitionIds(ctx, request.OrgId, env.ProjectId, env.EnvTypeId, env.Id, tx)
			if err != nil {
				return nil, err
			}
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
			CreatedAt:        def.CreatedAt,
			Dependencies:     deps,
			Coprovisioned:    coprovisioned,
			Description:      def.Description.Ref(),
			Id:               def.DefinitionId,
			ModuleInputs:     def.ModuleInputs,
			ModuleParams:     moduleParams,
			ModuleSource:     def.ModuleSource,
			ModuleSourceCode: def.ModuleSourceCode.Ref(),
			OrgId:            def.OrgId,
			ProviderMapping:  def.ProviderMapping,
			ResourceType:     def.ResourceType,
			Rules:            rules,
			UpdatedAt:        def.UpdatedAt,
			VersionId:        def.VersionId,
		})
	}

	return GenerateInternalModuleCatalogue200JSONResponse(out), nil
}
