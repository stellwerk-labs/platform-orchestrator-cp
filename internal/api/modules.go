package api

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	orchestratordp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v2/genclient"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

const (
	inlineModuleSource              = "inline"
	maximumInlineModuleSourceLength = 10_000
)

func apiMdFromDbMd(mp model.ModuleDefinitionVersion) Module {
	deps := make(map[string]ModuleDependencyManifest, len(mp.Dependencies))
	for s, dependency := range mp.Dependencies {
		deps[s] = ModuleDependencyManifest{
			Type:   dependency.Type,
			Class:  dependency.Class.Ref(),
			Id:     dependency.Id.Ref(),
			Params: dependency.Params,
		}
	}
	cops := make([]ModuleCoProvisionManifest, len(mp.CoProvisioned))
	for i, c := range mp.CoProvisioned {
		cops[i] = ModuleCoProvisionManifest{
			Type:                      c.Type,
			Id:                        c.Id.Ref(),
			Class:                     c.Class.Ref(),
			Params:                    c.Params,
			IsDependentOnCurrent:      c.IsDependentOnCurrent,
			CopyDependentsFromCurrent: c.CopyDependentsFromCurrent,
		}
	}
	mps := make(map[string]ModuleParamItem, len(mp.ModuleParams))
	for s, param := range mp.ModuleParams {
		mps[s] = ModuleParamItem{
			Description: ref.RefStringEmptyNil(param.Description),
			IsOptional:  param.IsOptional,
			Type:        ModuleParamItemType(param.Type),
		}
	}
	if mp.ModuleInputs == nil {
		mp.ModuleInputs = make(map[string]interface{})
	}
	if mp.ProviderMapping == nil {
		mp.ProviderMapping = make(map[string]string)
	}
	return Module{
		CreatedAt:        mp.CreatedAt,
		Dependencies:     deps,
		Coprovisioned:    cops,
		Description:      mp.Description.Ref(),
		Id:               mp.DefinitionId,
		ModuleInputs:     mp.ModuleInputs,
		ModuleParams:     mps,
		ModuleSource:     mp.ModuleSource,
		ModuleSourceCode: mp.ModuleSourceCode.Ref(),
		OrgId:            mp.OrgId,
		ProviderMapping:  mp.ProviderMapping,
		ResourceType:     mp.ResourceType,
		UpdatedAt:        mp.UpdatedAt,
		VersionId:        mp.VersionId,
	}
}

func apiMdvFromApiMd(md Module) ModuleVersion {
	return ModuleVersion{
		Coprovisioned:    md.Coprovisioned,
		CreatedAt:        md.UpdatedAt,
		Description:      md.Description,
		Dependencies:     md.Dependencies,
		ModuleInputs:     md.ModuleInputs,
		ModuleParams:     md.ModuleParams,
		ModuleSource:     md.ModuleSource,
		ModuleSourceCode: md.ModuleSourceCode,
		ProviderMapping:  md.ProviderMapping,
		ResourceType:     md.ResourceType,
		VersionId:        md.VersionId,
	}
}

func apiMdSummaryFromDbMd(mp model.ModuleDefinitionVersion) ModuleSummary {
	if mp.ProviderMapping == nil {
		mp.ProviderMapping = make(map[string]string)
	}
	return ModuleSummary{
		CreatedAt:       mp.CreatedAt,
		Description:     mp.Description.Ref(),
		Id:              mp.DefinitionId,
		ModuleSource:    mp.ModuleSource,
		OrgId:           mp.OrgId,
		ProviderMapping: mp.ProviderMapping,
		ResourceType:    mp.ResourceType,
		UpdatedAt:       mp.UpdatedAt,
		VersionId:       mp.VersionId,
	}
}

func apiMdvSummaryFromDbMd(mp model.ModuleDefinitionVersion) ModuleVersionSummary {
	if mp.ProviderMapping == nil {
		mp.ProviderMapping = make(map[string]string)
	}
	return ModuleVersionSummary{
		CreatedAt:       mp.UpdatedAt,
		Description:     mp.Description.Ref(),
		ModuleSource:    mp.ModuleSource,
		ProviderMapping: mp.ProviderMapping,
		ResourceType:    mp.ResourceType,
		VersionId:       mp.VersionId,
	}
}

func dbMddFromApiMdd(d ModuleDependencyManifest) model.ModuleDefinitionDependency {
	return model.ModuleDefinitionDependency{
		Type:   d.Type,
		Id:     opt.OfRef(d.Id),
		Class:  opt.OfRef(d.Class),
		Params: d.Params,
	}
}

func dbMdcFromApiMdc(c ModuleCoProvisionManifest) model.ModuleDefinitionCoProvision {
	return model.ModuleDefinitionCoProvision{
		Type:                      c.Type,
		Id:                        opt.OfRef(c.Id),
		Class:                     opt.OfRef(c.Class),
		IsDependentOnCurrent:      c.IsDependentOnCurrent,
		CopyDependentsFromCurrent: c.CopyDependentsFromCurrent,
		Params:                    c.Params,
	}
}

func (s *Server) ListModules(ctx context.Context, request ListModulesRequestObject) (ListModulesResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleRead); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListModuleDefinitions(ctx, nil, request.OrgId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListModuleDefinitionsParams{
		ByResourceType: request.Params.ByResourceType,
	})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListModules404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list modules")
	}
	out := make([]ModuleSummary, len(page))
	for i, item := range page {
		out[i] = apiMdSummaryFromDbMd(item)
	}
	return ListModules200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func validateModuleSource(moduleSource string, moduleSourceCode *string) error {
	if moduleSource == inlineModuleSource && moduleSourceCode == nil {
		return fmt.Errorf("module source code must be defined when module source is '%s'", inlineModuleSource)
	} else if moduleSource != inlineModuleSource && moduleSourceCode != nil {
		return fmt.Errorf("module source code must not be defined when module source is not '%s'", inlineModuleSource)
	}
	if moduleSourceCode != nil {
		if len(*moduleSourceCode) > maximumInlineModuleSourceLength {
			return fmt.Errorf("inline module source code is limited to %d characters, move to an alternative module source type to avoid this limit", maximumInlineModuleSourceLength)
		}

		// hcl syntax validation
		parser := hclparse.NewParser()
		if _, diags := parser.ParseHCL([]byte(*moduleSourceCode), "terraform.tf"); diags != nil && diags.HasErrors() {
			return fmt.Errorf("module source code must have valid hcl syntax: %s", diags.Error())
		}
		return nil
	}
	// we support absolute file paths inside the runner
	if strings.HasPrefix(moduleSource, "/") {
		if !fs.ValidPath(strings.TrimPrefix(moduleSource, "/")) {
			return fmt.Errorf("module source looks like a file path, but must be a valid absolute path: '%s'", moduleSource)
		}
		return nil
	}
	// we support generic git
	if strings.HasPrefix(moduleSource, "git::") {
		if !strings.HasPrefix(moduleSource, "git::ssh://") && !strings.HasPrefix(moduleSource, "git::https://") {
			return fmt.Errorf("git-based module source must have git::ssh:// or git::https:// prefix")
		}
		return nil
	}
	// to reduce confusion, force github.com and bitbucket to use the git source
	if strings.HasPrefix(moduleSource, "github.com/") || strings.HasPrefix(moduleSource, "bitbucket.org/") {
		return fmt.Errorf("public git-based module source must have git::https:// prefix")
	}
	// anything else should be a module registry
	parts := strings.Split(moduleSource, "/")
	if len(parts) == 3 || (len(parts) > 3 && parts[3] == "") {
		return nil
	} else if len(parts) == 4 || len(parts) > 4 && parts[4] == "" {
		return nil
	} else if len(parts) > 1 {
		return fmt.Errorf("module source looks like a registry reference and should be (hostname/)namespace/name/provider(//..)")
	}
	return fmt.Errorf("module source must either be a module registry reference, a git:: repository, or an absolute path available in the runner")
}

var validTerraformIdentifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

func validateModuleInputsAndParamInputs(inputKeys iter.Seq[string], paramKeys iter.Seq[string]) error {
	keys := slices.Collect(inputKeys)
	for _, k := range keys {
		if !validTerraformIdentifier.MatchString(k) {
			return fmt.Errorf("module inputs: '%s' is not a valid identifier", k)
		}
	}
	for k := range paramKeys {
		if !validTerraformIdentifier.MatchString(k) {
			return fmt.Errorf("module parameters: '%s' is not a valid identifier", k)
		}
		if slices.Contains(keys, k) {
			return fmt.Errorf("module inputs and module parameters cannot have the same key: %s", k)
		}
	}
	return nil
}

func (s *Server) CreateModule(ctx context.Context, request CreateModuleRequestObject) (CreateModuleResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleWrite); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapModuleDefinitionId(request.Body.Id))
	if err := validateModuleSource(request.Body.ModuleSource, request.Body.ModuleSourceCode); err != nil {
		return CreateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("/module_source: " + err.Error())}, nil
	}

	if err := validateModuleInputsAndParamInputs(maps.Keys(request.Body.ModuleParams), maps.Keys(request.Body.ModuleInputs)); err != nil {
		return CreateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("/module_inputs: " + err.Error())}, nil
	}

	if err := ValidatePlaceholderSyntax(request.Body.ModuleInputs, PlaceholdersSupportedInModule); err != nil {
		return CreateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("invalid configuration: " + err.Error())}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		var referencedTypes = []string{request.Body.ResourceType}
		now := time.Now().UTC()
		deps := make(map[string]model.ModuleDefinitionDependency)
		for i, i2 := range request.Body.Dependencies {
			deps[i] = dbMddFromApiMdd(i2)
			referencedTypes = append(referencedTypes, i2.Type)
		}

		cops := make([]model.ModuleDefinitionCoProvision, 0, len(request.Body.Coprovisioned))
		for _, c := range request.Body.Coprovisioned {
			cops = append(cops, dbMdcFromApiMdc(c))
			referencedTypes = append(referencedTypes, c.Type)
		}

		prms := make(map[string]model.ModuleParam, len(request.Body.ModuleParams))
		for k, p := range request.Body.ModuleParams {
			prms[k] = model.ModuleParam{
				Type:        string(p.Type),
				IsOptional:  p.IsOptional,
				Description: ref.DerefOr(p.Description, ""),
			}
		}

		if missingTypes, err := s.checkTypesExist(ctx, tx, request.OrgId, referencedTypes); err != nil {
			return nil, errors.Wrap(err, "failed to check that all the referenced types exist")
		} else if len(missingTypes) > 0 {
			return CreateModule409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf("the following types referenced by the module do not exist as builtin or custom types: %v", missingTypes))}, nil
		}

		r, err := s.Database.CreateModuleDefinition(ctx, tx, &model.ModuleDefinitionVersion{
			OrgId:            request.OrgId,
			DefinitionId:     request.Body.Id,
			CreatedAt:        now,
			ResourceType:     request.Body.ResourceType,
			VersionId:        uuid.NewString(),
			UpdatedAt:        now,
			Description:      opt.OfRef(request.Body.Description),
			ModuleSource:     request.Body.ModuleSource,
			ModuleSourceCode: opt.OfRef(request.Body.ModuleSourceCode),
			ModuleParams:     prms,
			ModuleInputs:     request.Body.ModuleInputs,
			Dependencies:     deps,
			CoProvisioned:    cops,
			ProviderMapping:  request.Body.ProviderMapping,
		})
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				err = model.NewErrConflict(me.Message)
			}
			if me, ok := model.IsErrConflict(err); ok {
				return CreateModule409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
			} else if me, ok := model.IsErrBadRequest(err); ok {
				return CreateModule400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create module")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}

		logger.Info("updated module to new version", logging.ZapModuleDefinitionVersionId(r.VersionId))
		return CreateModule201JSONResponse(apiMdFromDbMd(*r)), nil
	}
}

func (s *Server) DeleteModule(ctx context.Context, request DeleteModuleRequestObject) (DeleteModuleResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleWrite); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapModuleDefinitionId(request.ModuleId))

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		if r, err := s.DpClient.InternalCheckModuleUsageWithResponse(ctx, request.OrgId, request.ModuleId, &orchestratordp.InternalCheckModuleUsageParams{}); err != nil {
			return nil, errors.Wrap(err, "failed to make check usage request")
		} else if r.StatusCode() != http.StatusOK {
			return nil, errors.Errorf("unexpected status code %d from check usage request: %s", r.StatusCode(), string(r.Body))
		} else if len(r.JSON200.EnvIdsByProjectId) > 0 {
			return DeleteModule409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf(
				"module is used by one or more projects: %s",
				strings.Join(slices.Sorted(maps.Keys(r.JSON200.EnvIdsByProjectId)), ", "),
			))}, nil
		}

		if err := s.Database.DeleteModuleDefinition(ctx, tx, request.OrgId, request.ModuleId); err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return DeleteModule404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}
	}
	logger.Info("deleted module and all versions")
	return DeleteModule204Response{}, nil
}

func (s *Server) GetModule(ctx context.Context, request GetModuleRequestObject) (GetModuleResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleRead); err != nil {
		return nil, err
	}
	if r, err := s.Database.GetModuleDefinition(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetModule404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	} else {
		return GetModule200JSONResponse(apiMdFromDbMd(*r)), nil
	}
}

func (s *Server) UpdateModule(ctx context.Context, request UpdateModuleRequestObject) (UpdateModuleResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleWrite); err != nil {
		return nil, err
	}
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).With(logging.ZapModuleDefinitionId(request.ModuleId))

	if err := ValidatePlaceholderSyntax(ref.DerefOr(request.Body.ModuleInputs, map[string]interface{}{}), PlaceholdersSupportedInModule); err != nil {
		return UpdateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("invalid configuration: " + err.Error())}, nil
	}

	if tx, err := s.Database.BeginTx(ctx, nil); err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	} else {
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				logger.Error("failed to rollback transaction", zap.Error(err))
			}
		}()

		current, err := s.Database.GetModuleDefinition(ctx, tx, request.OrgId, request.ModuleId, model.GetModeForUpdate)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateModule404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to get module")
		}

		var zero ModuleUpdateBody
		if *request.Body == zero {
			return UpdateModule200JSONResponse(apiMdFromDbMd(*current)), nil
		}

		current.VersionId = uuid.NewString()
		current.UpdatedAt = time.Now().UTC()
		current.Description = opt.OfRef(request.Body.Description).OrOpt(current.Description)

		// allow the module source to be switched from remote source to code source and vise versa
		if request.Body.ModuleSource != nil {
			if err := validateModuleSource(*request.Body.ModuleSource, request.Body.ModuleSourceCode); err != nil {
				return UpdateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("/module_source: " + err.Error())}, nil
			}
			current.ModuleSource = *request.Body.ModuleSource
			current.ModuleSourceCode = opt.OfRef(request.Body.ModuleSourceCode)
		} else if request.Body.ModuleSourceCode != nil {
			if !current.ModuleSourceCode.IsSet() {
				return UpdateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("/module_source_code: cannot be set without module_source")}, nil
			}
			current.ModuleSourceCode = opt.OfRef(request.Body.ModuleSourceCode)
		}

		current.ModuleInputs = ref.DerefOr(request.Body.ModuleInputs, current.ModuleInputs)
		current.ProviderMapping = ref.DerefOr(request.Body.ProviderMapping, current.ProviderMapping)
		var referencedTypes = make([]string, 0)
		if request.Body.Dependencies != nil {
			current.Dependencies = make(map[string]model.ModuleDefinitionDependency)
			for i, i2 := range *request.Body.Dependencies {
				current.Dependencies[i] = dbMddFromApiMdd(i2)
				referencedTypes = append(referencedTypes, i2.Type)
			}
		}
		if request.Body.Coprovisioned != nil {
			current.CoProvisioned = make([]model.ModuleDefinitionCoProvision, 0, len(*request.Body.Coprovisioned))
			for _, c := range *request.Body.Coprovisioned {
				current.CoProvisioned = append(current.CoProvisioned, dbMdcFromApiMdc(c))
				referencedTypes = append(referencedTypes, c.Type)
			}
		}
		if request.Body.ModuleParams != nil {
			current.ModuleParams = make(map[string]model.ModuleParam)
			for k, p := range *request.Body.ModuleParams {
				current.ModuleParams[k] = model.ModuleParam{
					Type:        string(p.Type),
					IsOptional:  p.IsOptional,
					Description: ref.DerefOr(p.Description, ""),
				}
			}
		}

		if err := validateModuleInputsAndParamInputs(maps.Keys(current.ModuleInputs), maps.Keys(current.ModuleParams)); err != nil {
			return UpdateModule400JSONResponse{N400BadRequestJSONResponse: Generate400Response("/module_inputs: " + err.Error())}, nil
		}

		if missingTypes, err := s.checkTypesExist(ctx, tx, request.OrgId, referencedTypes); err != nil {
			return nil, errors.Wrap(err, "failed to check that all the referenced types exist")
		} else if len(missingTypes) > 0 {
			return UpdateModule409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf("the following types referenced by the module do not exist as builtin or custom types: %v", missingTypes))}, nil
		}

		r, err := s.Database.CreateModuleDefinitionVersion(ctx, tx, current)
		if err != nil {
			if me, ok := model.IsErrNotFound(err); ok {
				return UpdateModule404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
			} else if me, ok := model.IsErrBadRequest(err); ok {
				return UpdateModule400JSONResponse{N400BadRequestJSONResponse: Generate400FromModelErr(me)}, nil
			}
			return nil, errors.Wrap(err, "failed to create module version")
		} else if err := tx.Commit(); err != nil {
			return nil, errors.Wrap(err, "failed to commit transaction")
		}

		logger.Info("updated module to new version", logging.ZapModuleDefinitionVersionId(r.VersionId))
		return UpdateModule200JSONResponse(apiMdFromDbMd(*r)), nil
	}
}

func (s *Server) checkTypesExist(ctx context.Context, tx model.TxWithCommit, orgId string, typesWithDuplicated []string) (missingTypes []string, err error) {
	slices.Sort(typesWithDuplicated)
	uniqueReferencedTypes := slices.Compact(typesWithDuplicated)
	if len(uniqueReferencedTypes) > 0 {
		if types, err := s.Database.BulkGetResourceTypes(ctx, tx, orgId, uniqueReferencedTypes); err != nil {
			return nil, errors.Wrap(err, "failed to check that all the referenced types exist")
		} else {
			if len(types) < len(uniqueReferencedTypes) {
				typesNotFound := slices.DeleteFunc(uniqueReferencedTypes, func(tId string) bool {
					return slices.ContainsFunc(types, func(t model.ResourceType) bool {
						return t.Id == tId
					})
				})
				return typesNotFound, nil
			}
		}
	}
	return nil, nil
}

func (s *Server) ListModuleVersions(ctx context.Context, request ListModuleVersionsRequestObject) (ListModuleVersionsResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleRead); err != nil {
		return nil, err
	}
	if _, err := s.Database.GetModuleDefinition(ctx, nil, request.OrgId, request.ModuleId, model.GetModeDefault); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListModuleVersions404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get module")
	}
	page, next, err := s.Database.ListModuleDefinitionVersions(ctx, nil, request.OrgId, request.ModuleId, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListModuleDefinitionVersionsParams{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list modules")
	}
	out := make([]ModuleVersionSummary, len(page))
	for i, item := range page {
		out[i] = apiMdvSummaryFromDbMd(item)
	}
	return ListModuleVersions200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func (s *Server) GetModuleVersion(ctx context.Context, request GetModuleVersionRequestObject) (GetModuleVersionResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, PermissionModuleRead); err != nil {
		return nil, err
	}
	if r, err := s.Database.GetModuleDefinitionVersion(ctx, nil, request.OrgId, request.ModuleId, request.ModuleVersionId.String()); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetModuleVersion404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, err
	} else {
		return GetModuleVersion200JSONResponse(apiMdvFromApiMd(apiMdFromDbMd(*r))), nil
	}
}
