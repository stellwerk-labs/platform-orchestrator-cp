package integrationtests

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestRules(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id
	projectId := MustCreateProject(t, MustServerClient(t), orgId, strings.ToLower("project-"+rand.Text())).Id
	envTypeId := MustCreateEnvType(t, client, orgId, strings.ToLower("et-"+rand.Text())).Id
	_ = MustCreateRunnerWithRule(t, client, orgId, envTypeId, projectId, strings.ToLower("runner-"+rand.Text()))
	envId := MustCreateEnv(t, client, orgId, envTypeId, projectId, strings.ToLower("env-"+rand.Text())).Id

	t.Run("no definition rules exist", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	t.Run("can't get a rule that doesn't exist", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(t.Context(), orgId, uuid.New())
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module rule not found", res.JSON404.Message)
		}
	})

	t.Run("can't delete a rule that doesn't exist", func(t *testing.T) {
		res, err := client.DeleteModuleRuleInOrgWithResponse(t.Context(), orgId, uuid.New())
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module rule not found", res.JSON404.Message)
		}
	})

	t.Run("can't create a rule for a module that doesn't exist", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId: "unknown",
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module not found", res.JSON400.Message)
		}
	})

	resourceType := "s3"
	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.ResourceTypeCreateBody{
			Id:           resourceType,
			OutputSchema: map[string]interface{}{"type": "object"},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	moduleId := "def-" + strings.ToLower(rand.Text())
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.ModuleCreateBody{
			Id:           moduleId,
			ModuleSource: "/some/module/path",
			ResourceType: resourceType,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	var createdRule genclient.Rule
	t.Run("can create a rule", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId: moduleId,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			createdRule = *res.JSON201
			assert.Equal(t, genclient.Rule{
				CreatedAt:     createdRule.CreatedAt,
				ModuleId:      moduleId,
				Id:            createdRule.Id,
				OrgId:         orgId,
				ResourceClass: "default",
				ResourceId:    nil,
				ResourceType:  resourceType,
			}, createdRule)
		}
	})

	t.Run("can get a rule I created", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(t.Context(), orgId, createdRule.Id)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, createdRule, *res.JSON200)
		}
	})

	t.Run("can't create the same rule again", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId: moduleId,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, fmt.Sprintf("rule conflicts with existing rule '%s' for definition '%s'", createdRule.Id, moduleId), res.JSON409.Message)
		}
	})

	t.Run("but can create rules with different class or specific id", func(t *testing.T) {
		for _, r := range []genclient.RuleCreateBody{
			{ModuleId: moduleId, ResourceClass: ref.Ref("non-default")},
			{ModuleId: moduleId, ResourceClass: ref.Ref("non-default"), ResourceId: ref.Ref("specific")},
			{ModuleId: moduleId, ResourceId: ref.Ref("specific")},
			{ModuleId: moduleId, ProjectId: &projectId},
		} {
			res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, r)
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			}
		}
	})

	t.Run("can list rules by type", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByResourceType: &resourceType})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 5)
		}
		res, err = client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByResourceType: ref.Ref(resourceType + resourceType)})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}
	})

	t.Run("can list rules by module", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByModuleId: &moduleId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 5)
		}
		res, err = client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByModuleId: ref.Ref(moduleId + moduleId)})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}
	})

	t.Run("rules paginate correctly", func(t *testing.T) {
		first, nextToken, seen := true, "", make([]string, 0)
		for first || nextToken != "" {
			first = false
			res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{PerPage: ref.Ref(1), Page: ref.RefStringEmptyNil(nextToken)})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			t.Logf("fetched page of %d with token %s", len(res.JSON200.Items), nextToken)
			assert.LessOrEqual(t, len(res.JSON200.Items), 6)
			for _, item := range res.JSON200.Items {
				assert.NotContains(t, seen, item.Id)
				seen = append(seen, fmt.Sprintf("%s,%s,%s", item.ResourceType, item.ResourceClass, opt.OfRef(item.ResourceId).Or("?")))
			}
			nextToken = ref.DerefOr(res.JSON200.NextPageToken, "")
		}
		sort.Strings(seen)
		assert.Equal(t, []string{
			"s3,default,?",
			"s3,default,?",
			"s3,default,specific",
			"s3,non-default,?",
			"s3,non-default,specific",
		}, seen)
	})

	t.Run("can delete a rule", func(t *testing.T) {
		res, err := client.DeleteModuleRuleInOrgWithResponse(t.Context(), orgId, createdRule.Id)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cant get a deleted rule", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(t.Context(), orgId, createdRule.Id)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "module rule not found", res.JSON404.Message)
		}
	})

	t.Run("can list rules by project", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByProjectId: &projectId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.Equal(t, moduleId, res.JSON200.Items[0].ModuleId)
		}
	})

	t.Run("can list rules by env - empty results", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByProjectId: &projectId, ByEnvId: &envId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}
	})

	t.Run("can create a rule for project+env", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:  moduleId,
			ProjectId: &projectId,
			EnvId:     &envId,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, moduleId, res.JSON201.ModuleId)
			assert.Equal(t, projectId, *res.JSON201.ProjectId)
			assert.Equal(t, envId, *res.JSON201.EnvId)
		}
	})

	t.Run("can list rules by env - non empty results", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByProjectId: &projectId, ByEnvId: &envId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.Equal(t, moduleId, res.JSON200.Items[0].ModuleId)
			assert.Equal(t, projectId, *res.JSON200.Items[0].ProjectId)
			assert.Equal(t, envId, *res.JSON200.Items[0].EnvId)
		}
	})

	t.Run("can list rules by project - 2 results", func(t *testing.T) {
		res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByProjectId: &projectId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 2)
		}
	})

	t.Run("deleting the definition deletes the rules", func(t *testing.T) {
		{
			res, err := client.DeleteModuleWithResponse(t.Context(), orgId, moduleId)
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusNoContent, res.StatusCode(), string(res.Body))
			}
		}
		{
			res, err := client.ListModuleRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListModuleRulesInOrgParams{ByModuleId: &moduleId})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
				assert.Empty(t, res.JSON200.Items)
			}
		}
	})
}
