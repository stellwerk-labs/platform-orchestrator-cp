package api

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func (s *Server) ListAvailableResourceTypes(ctx context.Context, request ListAvailableResourceTypesRequestObject) (ListAvailableResourceTypesResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgReadAuthorization(ctx, uid, request.OrgId); err != nil {
		return nil, err
	}
	page, next, err := s.Database.ListAvailableResourceTypes(ctx, nil, request.OrgId, request.ProjectId, request.EnvId,
		ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100),
		model.ListAvailableResourceTypeParams{TypeId: opt.OfRef(request.Params.TypeId), IncludeNonDeveloperAccessible: opt.OfRef(request.Params.IncludeNonDeveloperAccessible)})
	if err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return ListAvailableResourceTypes404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to list available resource types")
	}

	// Now we need to optimise this list by factoring out the project and env ids so that we only include the most
	// specific combination for this query. We don't want to include a project level option if there is already an env
	// level option as well even though both technically match.
	out := make([]AvailableResourceType, len(page))
	for i, item := range page {

		maxSpecificityPerResourceKind := make(map[resourceKind]int)
		maxPerResourceKind := make(map[resourceKind]model.Option)
		for _, option := range item.Options {
			kind := resourceKind{Type: item.Id, Class: option.ResourceClass, Id: opt.OfNonZero(option.ResourceId)}
			specificity := calculateSpecificity(opt.OfRef(option.EnvId), opt.OfRef(option.ProjectId), opt.OfRef(option.EnvTypeId))
			if m, ok := maxSpecificityPerResourceKind[kind]; !ok || specificity > m {
				maxSpecificityPerResourceKind[kind] = specificity
				maxPerResourceKind[kind] = option
			}
		}

		sortedOptions := slices.SortedFunc(maps.Values(maxPerResourceKind), func(a model.Option, b model.Option) int {
			if c := strings.Compare(a.ResourceClass, b.ResourceClass); c != 0 {
				return c
			}
			return strings.Compare(a.ResourceId, b.ResourceId)
		})

		out[i] = AvailableResourceType{
			Id:           item.Id,
			Description:  ref.RefStringEmptyNil(item.Description),
			OutputSchema: item.OutputsSchema,
			Options: slices.Collect(func(yield func(AvailableResourceTypeOption) bool) {
				for _, option := range sortedOptions {
					params := make(map[string]ModuleParamItem, len(option.ModuleParams))
					for k, param := range option.ModuleParams {
						params[k] = ModuleParamItem{
							Type:        ModuleParamItemType(param.Type),
							IsOptional:  param.IsOptional,
							Description: ref.RefStringEmptyNil(param.Description),
						}
					}
					yield(AvailableResourceTypeOption{
						ModuleId:      option.DefinitionId,
						ModuleParams:  params,
						ResourceClass: option.ResourceClass,
						ResourceId:    ref.RefStringEmptyNil(option.ResourceId),
						RuleId:        option.RuleId,
					})
				}
			}),
		}
	}
	return ListAvailableResourceTypes200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}
