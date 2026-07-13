package integrationtests

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestDefinitions(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id
	s3ModuleId := "my-s3-module"

	provType := "aws"
	provId := "default"
	{
		res, err := client.CreateModuleProviderWithResponse(t.Context(), orgId, genclient.CreateModuleProviderJSONRequestBody{
			Source:            "hashicorp/aws",
			ProviderType:      provType,
			Id:                provId,
			VersionConstraint: "~> 4.0",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	orgResourceType := "s3"
	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
			Id:           orgResourceType,
			Description:  ref.Ref("Some resource Type"),
			OutputSchema: map[string]interface{}{},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	t.Run("unknown resource-type", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId+orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: "unknown",
			ModuleSource: "/modules/my-s3",
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "the following types referenced by the module do not exist as builtin or custom types: [unknown]", res.JSON409.Message)
		}
	})

	t.Run("unknown resource-types in definition dependencies and coprovisioned", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: orgResourceType,
			ModuleSource: "/modules/my-s3",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
			Dependencies: map[string]genclient.ModuleDependencyManifest{
				"dep-one": {
					Type: "unknown",
				},
				"dep-two": {
					Type: orgResourceType,
				},
				"dep-three": {
					Type: "unknown",
				},
			},
			Coprovisioned: []genclient.ModuleCoProvisionManifest{
				{Type: orgResourceType}, {Type: "unknown"}, {Type: "unknown2"}, {Type: "unknown2"},
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "the following types referenced by the module do not exist as builtin or custom types: [unknown unknown2]", res.JSON409.Message)
		}
	})

	t.Run("unknown definition", func(t *testing.T) {
		{
			res, err := client.GetModuleWithResponse(t.Context(), orgId, "my-def")
			if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
				assert.Equal(t, "module not found", res.JSON404.Message)
			}
		}
		{
			res, err := client.UpdateModuleWithResponse(t.Context(), orgId, "my-def", genclient.UpdateModuleJSONRequestBody{})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
				assert.Equal(t, "module not found", res.JSON404.Message)
			}
		}
		{
			res, err := client.DeleteModuleWithResponse(t.Context(), orgId, "my-def")
			if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
				assert.Equal(t, "module not found", res.JSON404.Message)
			}
		}
	})

	t.Run("empty list", func(t *testing.T) {
		res, err := client.ListModulesWithResponse(t.Context(), orgId, &genclient.ListModulesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	t.Run("unknown providers", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: orgResourceType,
			ModuleSource: "/modules/my-s3",
			ProviderMapping: map[string]string{
				"aws": "aws.unknown",
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "invalid provider mapping: no provider exists with type aws and id unknown", res.JSON400.Message)
		}
	})

	var createdRes genclient.Module
	t.Run("create definition", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: orgResourceType,
			ModuleSource: "/modules/my-s3",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
			ModuleParams: map[string]genclient.ModuleParamItem{
				"something": {
					Type:        "string",
					Description: ref.Ref("Some description"),
					IsOptional:  true,
				},
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			createdRes = *res.JSON201

			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.UpdatedAt)
			assert.NotEmpty(t, res.JSON201.VersionId)
			res.JSON201.CreatedAt = time.Time{}
			res.JSON201.UpdatedAt = time.Time{}
			res.JSON201.VersionId = ""
			assert.Equal(t, genclient.Module{
				OrgId:        orgId,
				Id:           s3ModuleId,
				ResourceType: "s3",
				ModuleSource: "/modules/my-s3",
				ProviderMapping: map[string]string{
					provType: provType + "." + provId,
				},
				Dependencies:  map[string]genclient.ModuleDependencyManifest{},
				Coprovisioned: []genclient.ModuleCoProvisionManifest{},
				ModuleInputs:  map[string]interface{}{},
				ModuleParams: map[string]genclient.ModuleParamItem{
					"something": {
						Type:        "string",
						Description: ref.Ref("Some description"),
						IsOptional:  true,
					},
				},
			}, *res.JSON201)

		}
	})

	t.Run("cannot create another definition with the same id", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: orgResourceType,
			ModuleSource: "/modules/my-s3",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "HTTP-409", res.JSON409.Error)
			assert.Equal(t, "module definition with id my-s3-module already exists", res.JSON409.Message)
		}
	})

	t.Run("get definition", func(t *testing.T) {
		res, err := client.GetModuleWithResponse(t.Context(), orgId, "my-s3-module")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, createdRes, *res.JSON200)
		}
	})

	t.Run("can list definition", func(t *testing.T) {
		res, err := client.ListModulesWithResponse(t.Context(), orgId, &genclient.ListModulesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			item := res.JSON200.Items[0]
			assert.NotEmpty(t, item.CreatedAt)
			assert.NotEmpty(t, item.UpdatedAt)
			assert.NotEmpty(t, item.VersionId)
			item.CreatedAt = time.Time{}
			item.UpdatedAt = time.Time{}
			item.VersionId = ""
			assert.Equal(t, genclient.ModuleSummary{
				OrgId:        orgId,
				Id:           s3ModuleId,
				ResourceType: "s3",
				ModuleSource: "/modules/my-s3",
				ProviderMapping: map[string]string{
					provType: provType + "." + provId,
				},
			}, item)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	t.Run("can filter by resource type", func(t *testing.T) {
		res, err := client.ListModulesWithResponse(t.Context(), orgId, &genclient.ListModulesParams{
			ByResourceType: ref.Ref(orgResourceType),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
		res, err = client.ListModulesWithResponse(t.Context(), orgId, &genclient.ListModulesParams{
			ByResourceType: ref.Ref("unknown"),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	t.Run("can't delete provider", func(t *testing.T) {
		res, err := client.DeleteModuleProviderWithResponse(t.Context(), orgId, provType, provId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module versions are still using this provider: my-s3-module", res.JSON409.Message)
		}
	})

	t.Run("update definition with no updates", func(t *testing.T) {
		res, err := client.UpdateModuleWithResponse(t.Context(), orgId, "my-s3-module", genclient.UpdateModuleJSONRequestBody{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, createdRes, *res.JSON200)
		}
	})

	var updatedRes genclient.Module
	t.Run("update definition with no description updates", func(t *testing.T) {
		res, err := client.UpdateModuleWithResponse(t.Context(), orgId, "my-s3-module",
			genclient.UpdateModuleJSONRequestBody{
				ModuleInputs: &map[string]interface{}{"x": "y"},
			})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			res.JSON200.VersionId = ""
			res.JSON200.UpdatedAt = time.Time{}
			assert.Equal(t, genclient.Module{
				OrgId:        orgId,
				Id:           s3ModuleId,
				ResourceType: "s3",
				ModuleSource: "/modules/my-s3",
				ProviderMapping: map[string]string{
					provType: provType + "." + provId,
				},
				CreatedAt:     createdRes.CreatedAt,
				Dependencies:  map[string]genclient.ModuleDependencyManifest{},
				Coprovisioned: []genclient.ModuleCoProvisionManifest{},
				ModuleInputs:  map[string]interface{}{"x": "y"},
				ModuleParams: map[string]genclient.ModuleParamItem{
					"something": {
						Type:        "string",
						Description: ref.Ref("Some description"),
						IsOptional:  true,
					},
				},
			}, *res.JSON200)

		}
	})

	t.Run("unknown resource-types update in definition dependencies and coprovisioned", func(t *testing.T) {
		res, err := client.UpdateModuleWithResponse(t.Context(), orgId, "my-s3-module", genclient.UpdateModuleJSONRequestBody{
			Dependencies: &map[string]genclient.ModuleDependencyManifest{
				"thing": {
					Type:  "some-type",
					Class: ref.Ref("some-class"),
					Id:    ref.Ref("some-id"),
				},
				"other-thing": {
					Type: "another-type",
				},
				"other-other-thing": {
					Type: "some-type",
				},
			},
			Coprovisioned: &[]genclient.ModuleCoProvisionManifest{
				{Type: orgResourceType}, {Type: "unknown"}, {Type: "another-type"},
			},
			Description:     ref.Ref("new description"),
			ModuleInputs:    &map[string]interface{}{"x": "y"},
			ModuleSource:    ref.Ref("/modules/new/source"),
			ProviderMapping: &map[string]string{},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "the following types referenced by the module do not exist as builtin or custom types: [another-type some-type unknown]", res.JSON409.Message)
		}
	})

	t.Run("update definition with full updates", func(t *testing.T) {
		{
			res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
				Id:           "some-type",
				Description:  ref.Ref("Some resource Type"),
				OutputSchema: map[string]interface{}{},
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
		res, err := client.UpdateModuleWithResponse(t.Context(), orgId, "my-s3-module", genclient.UpdateModuleJSONRequestBody{
			Dependencies: &map[string]genclient.ModuleDependencyManifest{
				"thing": {
					Type:   "some-type",
					Class:  ref.Ref("some-class"),
					Id:     ref.Ref("some-id"),
					Params: map[string]interface{}{"param1": "value1"},
				},
			},
			Description:  ref.Ref("new description"),
			ModuleInputs: &map[string]interface{}{"x": "y"},
			ModuleParams: &map[string]genclient.ModuleParamItem{
				"y": {Type: "any"},
			},
			ModuleSource:    ref.Ref("/modules/new/source"),
			ProviderMapping: &map[string]string{},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			updatedRes = *res.JSON200
			assert.NotEmpty(t, res.JSON200.VersionId)
			assert.NotEqual(t, createdRes.VersionId, res.JSON200.VersionId)
			assert.Less(t, createdRes.UpdatedAt, res.JSON200.UpdatedAt)
			res.JSON200.VersionId = ""
			res.JSON200.UpdatedAt = time.Time{}
			assert.Equal(t, genclient.Module{
				OrgId:        orgId,
				Id:           s3ModuleId,
				ResourceType: "s3",
				ModuleSource: "/modules/new/source",
				Description:  ref.Ref("new description"),
				CreatedAt:    createdRes.CreatedAt,
				Dependencies: map[string]genclient.ModuleDependencyManifest{
					"thing": {
						Type:   "some-type",
						Class:  ref.Ref("some-class"),
						Id:     ref.Ref("some-id"),
						Params: map[string]interface{}{"param1": "value1"},
					},
				},
				Coprovisioned:   []genclient.ModuleCoProvisionManifest{},
				ModuleInputs:    map[string]interface{}{"x": "y"},
				ModuleParams:    map[string]genclient.ModuleParamItem{"y": {Type: "any"}},
				ProviderMapping: map[string]string{},
			}, *res.JSON200)
		}
	})

	t.Run("get updated module", func(t *testing.T) {
		res, err := client.GetModuleWithResponse(t.Context(), orgId, "my-s3-module")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, updatedRes, *res.JSON200)
		}
	})

	t.Run("get updated module version", func(t *testing.T) {
		res, err := client.GetModuleVersionWithResponse(t.Context(), orgId, "my-s3-module", uuid.MustParse(updatedRes.VersionId))
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, updatedRes.VersionId, res.JSON200.VersionId)
			assert.Equal(t, updatedRes.UpdatedAt, res.JSON200.CreatedAt)
			assert.Equal(t, updatedRes.ModuleParams, res.JSON200.ModuleParams)
			assert.Equal(t, updatedRes.ModuleInputs, res.JSON200.ModuleInputs)
			assert.Equal(t, updatedRes.Dependencies, res.JSON200.Dependencies)
			assert.Equal(t, updatedRes.Coprovisioned, res.JSON200.Coprovisioned)
			assert.Equal(t, updatedRes.ModuleSource, res.JSON200.ModuleSource)
			assert.Equal(t, updatedRes.ResourceType, res.JSON200.ResourceType)
			assert.Equal(t, updatedRes.ProviderMapping, res.JSON200.ProviderMapping)
		}
	})

	t.Run("updated module has multiple versions", func(t *testing.T) {
		res, err := client.ListModuleVersionsWithResponse(t.Context(), orgId, "my-s3-module", &genclient.ListModuleVersionsParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			if assert.Len(t, res.JSON200.Items, 3) {
				assert.Equal(t, updatedRes.VersionId, res.JSON200.Items[0].VersionId)
				assert.Equal(t, updatedRes.UpdatedAt, res.JSON200.Items[0].CreatedAt)
				assert.Equal(t, updatedRes.ModuleSource, res.JSON200.Items[0].ModuleSource)
				assert.Equal(t, updatedRes.ResourceType, res.JSON200.Items[0].ResourceType)
				assert.Equal(t, updatedRes.ProviderMapping, res.JSON200.Items[0].ProviderMapping)

				assert.Equal(t, createdRes.VersionId, res.JSON200.Items[2].VersionId)
				assert.Equal(t, createdRes.UpdatedAt, res.JSON200.Items[2].CreatedAt)
			}
		}
	})

	t.Run("can't delete resource type", func(t *testing.T) {
		res, err := client.DeleteResourceTypeWithResponse(t.Context(), orgId, orgResourceType)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "modules are still using this resource type", res.JSON409.Message)
		}
	})

	t.Run("delete module", func(t *testing.T) {
		res, err := client.DeleteModuleWithResponse(t.Context(), orgId, "my-s3-module")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("and module is not found", func(t *testing.T) {
		res, err := client.GetModuleWithResponse(t.Context(), orgId, "my-def")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module not found", res.JSON404.Message)
		}
	})

	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.ResourceTypeCreateBody{Id: "thing", Description: ref.Ref("Thing"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	t.Run("create and update module def with dependencies", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: "parent", ResourceType: "thing", ModuleSource: "acme/k8ss/generic@v1", Description: ref.Ref("parent def"),
			Dependencies: map[string]genclient.ModuleDependencyManifest{"child": {Type: "thing", Class: ref.Ref("child")}},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		assert.Len(t, res.JSON201.Dependencies, 1)

		res2, err := client.UpdateModuleWithResponse(t.Context(), orgId, "parent", genclient.UpdateModuleJSONRequestBody{
			Dependencies: &map[string]genclient.ModuleDependencyManifest{"child": {Type: "thing", Class: ref.Ref("child"), Id: ref.Ref("child-id")}},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res2.StatusCode(), string(res2.Body))
		assert.Equal(t, ref.Ref("child-id"), res2.JSON200.Dependencies["child"].Id)
	})

	t.Run("create and update module def with coprovisioned items", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: "parent2", ResourceType: "thing", ModuleSource: "acme/k8ss/generic@v1", Description: ref.Ref("parent def"),
			Coprovisioned: []genclient.ModuleCoProvisionManifest{{Type: "thing", Class: ref.Ref("remote"), Id: ref.Ref("remote-id"), CopyDependentsFromCurrent: true, IsDependentOnCurrent: true, Params: map[string]interface{}{"param1": "value1"}}},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		assert.Equal(t, genclient.ModuleCoProvisionManifest{Type: "thing", Class: ref.Ref("remote"), Id: ref.Ref("remote-id"), CopyDependentsFromCurrent: true, IsDependentOnCurrent: true, Params: map[string]interface{}{"param1": "value1"}}, res.JSON201.Coprovisioned[0])

		res2, err := client.UpdateModuleWithResponse(t.Context(), orgId, res.JSON201.Id, genclient.UpdateModuleJSONRequestBody{
			Coprovisioned: &[]genclient.ModuleCoProvisionManifest{{Type: "thing", CopyDependentsFromCurrent: false, IsDependentOnCurrent: false}},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res2.StatusCode(), string(res2.Body))
		assert.Equal(t, genclient.ModuleCoProvisionManifest{Type: "thing", CopyDependentsFromCurrent: false, IsDependentOnCurrent: false}, res2.JSON200.Coprovisioned[0])
	})

	t.Run("can create module with inline code", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: "inline-eg", ResourceType: "thing",
			ModuleSource: "inline",
			ModuleSourceCode: ref.Ref(`
output "foo" {
  value = "bar"
}
`),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		assert.Equal(t, `
output "foo" {
  value = "bar"
}
`, *res.JSON201.ModuleSourceCode)

		t.Run("can get", func(t *testing.T) {
			r, err := client.GetModuleWithResponse(t.Context(), orgId, res.JSON201.Id)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			assert.Equal(t, "inline", r.JSON200.ModuleSource)
			assert.Equal(t, *res.JSON201.ModuleSourceCode, *r.JSON200.ModuleSourceCode)
		})

		t.Run("can list", func(t *testing.T) {
			r, err := client.ListModulesWithResponse(t.Context(), orgId, &genclient.ListModulesParams{})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
			i := slices.IndexFunc(r.JSON200.Items, func(s genclient.ModuleSummary) bool {
				return s.Id == res.JSON201.Id
			})
			if assert.GreaterOrEqual(t, i, 0) {
				assert.Equal(t, "inline", r.JSON200.Items[i].ModuleSource)
			}
		})
	})

	t.Run("can't conflict params and inputs", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: "conflict", ResourceType: "thing",
			ModuleSource: "acme/k8ss/generic@v1",
			ModuleParams: map[string]genclient.ModuleParamItem{"x": {Type: "string"}},
			ModuleInputs: map[string]interface{}{"x": "y"},
		})
		require.NoError(t, err)
		if assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "/module_inputs: module inputs and module parameters cannot have the same key: x", res.JSON400.Message)
		}
	})

	for _, i := range []int{0, 2, 2_000, 10_000} {
		t.Run(fmt.Sprintf("can create an inline module with %d chars", i), func(t *testing.T) {
			res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
				Id: fmt.Sprintf("inline-%d", i), ResourceType: "thing",
				ModuleSource:     "inline",
				ModuleSourceCode: ref.Ref(strings.Repeat(" ", i)),
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, strings.Repeat(" ", i), *res.JSON201.ModuleSourceCode)
		})
	}

	t.Run("cannot create a module with a module source that is too long", func(t *testing.T) {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: "inline-too-long", ResourceType: "thing",
			ModuleSource:     "inline",
			ModuleSourceCode: ref.Ref(strings.Repeat(" ", 10_001)),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body))
		assert.Equal(t, "/module_source: inline module source code is limited to 10000 characters, move to an alternative module source type to avoid this limit", res.JSON400.Message)
	})

}
