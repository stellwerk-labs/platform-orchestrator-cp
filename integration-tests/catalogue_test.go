package integrationtests

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/errcodes"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestModuleCatalogueApi(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)

	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id
	envType := MustCreateEnvType(t, client, orgId, "dev").Id
	projectId := MustCreateProject(t, client, orgId, "my-project").Id
	_ = MustCreateRunnerWithRule(t, client, orgId, envType, projectId, "runner-"+strings.ToLower(uuid.NewString()))
	envId := MustCreateEnv(t, client, orgId, envType, projectId, "my-env").Id

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

	var egDefinition string
	for i := 0; i < 10; i++ {
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id: fmt.Sprintf("def-%d", i), ModuleSource: "/some/module/path", ResourceType: orgResourceType,
			ProviderMapping: map[string]string{
				"banana": provType + "." + provId,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		if egDefinition == "" {
			egDefinition = res.JSON201.Id + "@" + res.JSON201.VersionId
		}
	}

	t.Run("generate catalogue with empty org", func(t *testing.T) {
		res, err := internalClient.GenerateInternalModuleCatalogueWithResponse(t.Context(), orgId, projectId, envId, genclient.InternalModuleCatalogueGenerateBody{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Providers)
			assert.Empty(t, res.JSON200.Modules)
		}
	})

	for i, b := range []genclient.RuleCreateBody{
		// rules with the default class and no id
		{ProjectId: ref.Ref(projectId)},
		{ProjectId: ref.Ref(projectId), EnvId: ref.Ref(envId)},
		{EnvTypeId: ref.Ref(envType)},
		{ProjectId: ref.Ref(projectId), EnvTypeId: ref.Ref(envType)},
		// rules for a specific id
		{ResourceId: ref.Ref("specific-1")},
		{ResourceId: ref.Ref("specific-1"), ProjectId: ref.Ref(projectId)},
		// rules for non default class
		{ResourceClass: ref.Ref("non-default")},
		{ResourceClass: ref.Ref("non-default"), EnvTypeId: ref.Ref(envType)},
	} {
		b.ModuleId = fmt.Sprintf("def-%d", i%10)
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, b)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	for _, r := range []genclient.RuleCreateBody{} {
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, r)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	t.Run("generate catalogue", func(t *testing.T) {
		res, err := internalClient.GenerateInternalModuleCatalogueWithResponse(t.Context(), orgId, projectId, envId, genclient.InternalModuleCatalogueGenerateBody{
			PinnedModuleVersions: []string{egDefinition},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			if assert.Len(t, res.JSON200.Providers, 1) {
				assert.Equal(t, provType, res.JSON200.Providers[0].ProviderType)
				assert.Equal(t, provId, res.JSON200.Providers[0].Id)
			}
			// clear out uuids and sort the rules so we can test the output a bit better
			for i, definition := range res.JSON200.Modules {
				for r, rule := range definition.Rules {
					rule.RuleId = uuid.Nil
					res.JSON200.Modules[i].Rules[r] = rule
				}
				res.JSON200.Modules[i] = definition
			}
			if assert.Len(t, res.JSON200.Modules, 4) {
				assert.Equal(t, "def-1", res.JSON200.Modules[0].Id)
				assert.Equal(t, []genclient.InternalModuleCatalogueModuleRule{
					{ResourceClass: "default", ProjectId: ref.Ref(projectId), EnvId: ref.Ref(envId)},
				}, res.JSON200.Modules[0].Rules)
				assert.Equal(t, "def-5", res.JSON200.Modules[1].Id)
				assert.Equal(t, []genclient.InternalModuleCatalogueModuleRule{
					{ResourceId: ref.Ref("specific-1"), ResourceClass: "default", ProjectId: ref.Ref(projectId)},
				}, res.JSON200.Modules[1].Rules)
				assert.Equal(t, "def-7", res.JSON200.Modules[2].Id)
				assert.Equal(t, []genclient.InternalModuleCatalogueModuleRule{
					{ResourceClass: "non-default", EnvTypeId: ref.Ref(envType)},
				}, res.JSON200.Modules[2].Rules)
				assert.Equal(t, "def-0", res.JSON200.Modules[3].Id)
			}
		}
	})

	t.Run("generate catalogue pinned only", func(t *testing.T) {
		res, err := internalClient.GenerateInternalModuleCatalogueWithResponse(t.Context(), orgId, projectId, envId, genclient.InternalModuleCatalogueGenerateBody{
			PinnedModuleVersions: []string{egDefinition},
			AreRulesIgnored:      true,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			if assert.Len(t, res.JSON200.Providers, 1) {
				assert.Equal(t, provType, res.JSON200.Providers[0].ProviderType)
				assert.Equal(t, provId, res.JSON200.Providers[0].Id)
			}
			if assert.Len(t, res.JSON200.Modules, 1) {
				assert.Equal(t, "def-0", res.JSON200.Modules[0].Id)
				assert.Empty(t, res.JSON200.Modules[0].Rules)
			}
		}
	})

	t.Run("with deleted provider", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			r, err := client.UpdateModuleWithResponse(t.Context(), orgId, fmt.Sprintf("def-%d", i), genclient.UpdateModuleJSONRequestBody{
				ProviderMapping: &map[string]string{},
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
		}

		r, err := client.DeleteModuleProviderWithResponse(t.Context(), orgId, provType, provId)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, r.StatusCode(), string(r.Body))
		res, err := internalClient.GenerateInternalModuleCatalogueWithResponse(t.Context(), orgId, projectId, envId, genclient.InternalModuleCatalogueGenerateBody{
			PinnedModuleVersions: []string{egDefinition},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, string(errcodes.PinnedModuleMissingProvider), res.JSON409.Error)
			assert.Equal(t, &map[string]interface{}{"missing_providers": []interface{}{"aws.default"}}, res.JSON409.Details)
		}
	})

}
