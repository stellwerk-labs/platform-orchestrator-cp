package integrationtests

import (
	"crypto/rand"
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

func TestEnvs(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgId := MustCreateOrg(t, internalClient).Id
	envType := "development"
	projectId := "project-" + strings.ToLower(rand.Text())
	var projectOne, projectTwo genclient.Project
	projectIdTwo := "project-" + strings.ToLower(rand.Text())
	envNoDisplayNameId := "env-" + strings.ToLower(rand.Text())
	envDisplayNameId := "env-" + strings.ToLower(rand.Text())
	var envNoDisplayName, envWithDisplayName *genclient.Environment
	var localizedRunnerId, orgWideRunnerId string
	{
		etRes, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.EnvironmentTypeCreateBody{Id: envType})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, etRes.StatusCode())

		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectId})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		projectOne = *res.JSON201

		res, err = client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectIdTwo})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		projectTwo = *res.JSON201
	}

	t.Run("list environments in a non-existing organization returns 404", func(t *testing.T) {
		res, err := client.ListEnvironmentsInOrgWithResponse(t.Context(), "org-does-not-exist", &genclient.ListEnvironmentsInOrgParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot create an environment in a non-existing project", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, "test-project-id", genclient.EnvironmentCreateBody{Id: envNoDisplayNameId, EnvTypeId: envType})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("empty list", func(t *testing.T) {
		res, err := client.ListEnvironmentsWithResponse(t.Context(), orgId, projectId, &genclient.ListEnvironmentsParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	t.Run("empty list in the whole organization", func(t *testing.T) {
		res, err := client.ListEnvironmentsInOrgWithResponse(t.Context(), orgId, &genclient.ListEnvironmentsInOrgParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
			assert.Empty(t, res.JSON200.NextPageToken)
		}
	})

	iamClient := MustIamClient(t)
	tut := MustGenerateTestUserToken(t)
	userId := MustRegisterUser(t, iamClient, tut)
	internalIamClient := MustInternalIamClient(t)
	MustCreateMembershipInOrg(t, internalIamClient, orgId, "Viewer", "", userId)
	viewerUserClient := MustServerClientWithId(t, userId.String())

	t.Run("viewer cannot create environment", func(t *testing.T) {
		res, err := viewerUserClient.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: "env-" + strings.ToLower(rand.Text()), EnvTypeId: envType})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusForbidden, res.StatusCode(), string(res.Body)) {

			assert.Contains(t, string(res.Body), fmt.Sprintf(`"permission":"environment_write","resource":"project:%s"`, projectOne.Uuid.String()))
		}
	})

	// assign project write permission to the user
	MustCreateMembershipInOrg(t, internalIamClient, orgId, "Admin", "project:"+projectOne.Uuid.String(), userId)

	t.Run("can now create environment - no display name", func(t *testing.T) {
		assert.EventuallyWithT(t, func(collect *assert.CollectT) {
			res, err := viewerUserClient.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: envNoDisplayNameId, EnvTypeId: envType})
			if assert.NoError(t, err) && assert.Equal(collect, http.StatusCreated, res.StatusCode(), string(res.Body)) {
				envNoDisplayName = res.JSON201
				assert.Equal(t, envNoDisplayNameId, envNoDisplayName.Id)
				assert.Equal(t, projectId, envNoDisplayName.ProjectId)
				assert.Equal(t, envNoDisplayNameId, envNoDisplayName.DisplayName)
				assert.Equal(t, genclient.EnvironmentStatus("active"), envNoDisplayName.Status)
				assert.NotEmpty(t, envNoDisplayName.Uuid)
				assert.NotEmpty(t, envNoDisplayName.CreatedAt)
				assert.Empty(t, envNoDisplayName.RunnerId)
			}
		}, 30*time.Second, 3*time.Second, "failed to create environment")
	})

	{
		localizedRunnerId = MustCreateRunnerWithRule(t, client, orgId, "", projectId, "runner-"+strings.ToLower(rand.Text())).Id
		require.NotEmpty(t, localizedRunnerId)

		orgWideRunnerId = MustCreateRunnerWithRule(t, client, orgId, "", "", "runner-"+strings.ToLower(rand.Text())).Id
		require.NotEmpty(t, orgWideRunnerId)
	}

	t.Run("can update environment display name", func(t *testing.T) {
		res, err := viewerUserClient.UpdateEnvironmentWithResponse(t.Context(), orgId, projectId, envNoDisplayNameId, genclient.UpdateEnvironmentJSONRequestBody{
			DisplayName: "New Env Name",
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			envNoDisplayName.DisplayName = res.JSON200.DisplayName
			assert.Equal(t, "New Env Name", envNoDisplayName.DisplayName)
		}
	})

	t.Run("can retrieve the environmenty by uuid", func(t *testing.T) {
		res, err := internalClient.GetInternalEnvironmentByUuidWithResponse(t.Context(), orgId, envNoDisplayName.Uuid)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, envNoDisplayName.Id, res.JSON200.Id)
			assert.Equal(t, envNoDisplayName.ProjectId, res.JSON200.ProjectId)
			assert.Equal(t, envNoDisplayName.DisplayName, res.JSON200.DisplayName)
			assert.Equal(t, envNoDisplayName.Uuid, res.JSON200.Uuid)
			assert.Equal(t, envNoDisplayName.CreatedAt, res.JSON200.CreatedAt)
			if assert.NotNil(t, res.JSON200.ProjectUuid) {
				assert.Equal(t, projectOne.Uuid, *res.JSON200.ProjectUuid)
			}
		}
	})

	t.Run("cannot retrieve the same env uuid in another org", func(t *testing.T) {
		res, err := internalClient.GetInternalEnvironmentByUuidWithResponse(t.Context(), "not-existing-org", envNoDisplayName.Uuid)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
			assert.Equal(t, "environment not found", res.JSON404.Message)
		}
	})

	var mostScopedRunnerId = MustCreateRunnerWithRule(t, client, orgId, envType, projectId, "runner-most-scoped-"+strings.ToLower(rand.Text())).Id
	t.Run("can check for a refreshed environment runner without changing it", func(t *testing.T) {
		res, err := client.UpdateRunnerInAnEnvironmentWithResponse(t.Context(), orgId, projectId, envNoDisplayNameId, &genclient.UpdateRunnerInAnEnvironmentParams{DryRun: ref.Ref(true)})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.False(t, res.JSON200.Updated)
			assert.Equal(t, mostScopedRunnerId, res.JSON200.RunnerId)
		}
	})

	t.Run("the environment runner is still empty", func(t *testing.T) {
		res, err := client.GetEnvironmentWithResponse(t.Context(), orgId, projectId, envNoDisplayNameId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.RunnerId)
		}
	})

	t.Run("can refresh the environment runner", func(t *testing.T) {
		res, err := client.UpdateRunnerInAnEnvironmentWithResponse(t.Context(), orgId, projectId, envNoDisplayNameId, &genclient.UpdateRunnerInAnEnvironmentParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.True(t, res.JSON200.Updated)
			assert.Equal(t, mostScopedRunnerId, res.JSON200.RunnerId)
		}
	})

	t.Run("the environment runner did  change", func(t *testing.T) {
		res, err := client.GetEnvironmentWithResponse(t.Context(), orgId, projectId, envNoDisplayNameId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, mostScopedRunnerId, *res.JSON200.RunnerId)
			envNoDisplayName = res.JSON200
		}
	})

	t.Run("cannot refresh the runner of a non-existing environment", func(t *testing.T) {
		res, err := client.UpdateRunnerInAnEnvironmentWithResponse(t.Context(), orgId, projectId, "id-does-not-exist", &genclient.UpdateRunnerInAnEnvironmentParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
			assert.Equal(t, "environment not found", res.JSON404.Message)
		}
	})

	t.Run("can list environment", func(t *testing.T) {
		res, err := client.ListEnvironmentsWithResponse(t.Context(), orgId, projectId, &genclient.ListEnvironmentsParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			item := res.JSON200.Items[0]
			assert.Equal(t, *envNoDisplayName, item)
		}
	})

	t.Run("obtain 404 if the project does not exist listing envs", func(t *testing.T) {
		res, err := client.ListEnvironmentsWithResponse(t.Context(), orgId, "project-does-not-exist", &genclient.ListEnvironmentsParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("can list environments across projects by project uuid", func(t *testing.T) {
		res, err := internalClient.ListInternalEnvironmentsByProjectUuidWithResponse(t.Context(), orgId, projectOne.Uuid, &genclient.ListInternalEnvironmentsByProjectUuidParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			item := res.JSON200.Items[0]
			assert.Equal(t, *envNoDisplayName, item)
		}
	})

	t.Run("obtain 404 if the project does not exist listing envs by project uuid", func(t *testing.T) {
		res, err := internalClient.ListInternalEnvironmentsByProjectUuidWithResponse(t.Context(), orgId, uuid.New(), &genclient.ListInternalEnvironmentsByProjectUuidParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode())
		}
	})

	t.Run("can filter list by env type", func(t *testing.T) {
		res, err := client.ListEnvironmentsWithResponse(t.Context(), orgId, projectId, &genclient.ListEnvironmentsParams{ByEnvTypeId: &[]string{envType}})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			assert.Equal(t, envNoDisplayName.Id, res.JSON200.Items[0].Id)
		}
		res, err = client.ListEnvironmentsWithResponse(t.Context(), orgId, projectId, &genclient.ListEnvironmentsParams{ByEnvTypeId: &[]string{"does-not-exist"}})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}
	})

	t.Run("cannot create environment with same id in the same project", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: envNoDisplayName.Id, EnvTypeId: envType})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
		}
	})

	t.Run("can create environment with same id in another project", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, projectIdTwo, genclient.EnvironmentCreateBody{Id: envNoDisplayName.Id, EnvTypeId: envType})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			envNoDisplayName = res.JSON201
			assert.Equal(t, envNoDisplayNameId, envNoDisplayName.Id)
			assert.Equal(t, projectIdTwo, envNoDisplayName.ProjectId)
			assert.Equal(t, envNoDisplayNameId, envNoDisplayName.DisplayName)
			assert.Equal(t, genclient.EnvironmentStatus("active"), envNoDisplayName.Status)
			assert.NotEmpty(t, envNoDisplayName.Uuid)
			assert.NotEmpty(t, envNoDisplayName.CreatedAt)
			assert.Empty(t, envNoDisplayName.RunnerId)
		}
	})

	t.Run("can create environment - with display name", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: envDisplayNameId, EnvTypeId: envType, DisplayName: ref.Ref("My Awesome Env")})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			envWithDisplayName = res.JSON201
			assert.Equal(t, envDisplayNameId, envWithDisplayName.Id)
			assert.Equal(t, projectId, envWithDisplayName.ProjectId)
			assert.Equal(t, "My Awesome Env", envWithDisplayName.DisplayName)
			assert.NotEmpty(t, envWithDisplayName.Uuid)
			assert.NotEmpty(t, envWithDisplayName.CreatedAt)
			assert.NotEmpty(t, envWithDisplayName.UpdatedAt)
		}
	})

	t.Run("can get the environment", func(t *testing.T) {
		res, err := client.GetEnvironmentWithResponse(t.Context(), orgId, projectId, envDisplayNameId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, res.JSON200, envWithDisplayName)
		}
	})

	t.Run("can list environments in the whole organization", func(t *testing.T) {
		res, err := client.ListEnvironmentsInOrgWithResponse(t.Context(), orgId, &genclient.ListEnvironmentsInOrgParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 3)
			assert.Greater(t, slices.IndexFunc(res.JSON200.Items, func(e genclient.Environment) bool {
				return e.Id == envNoDisplayNameId && e.ProjectId == projectOne.Id && projectOne.Uuid == *e.ProjectUuid
			}), -1)
			assert.Greater(t, slices.IndexFunc(res.JSON200.Items, func(e genclient.Environment) bool {
				return e.Id == envDisplayNameId && e.ProjectId == projectOne.Id && projectOne.Uuid == *e.ProjectUuid
			}), -1)
			assert.Greater(t, slices.IndexFunc(res.JSON200.Items, func(e genclient.Environment) bool {
				return e.Id == envNoDisplayNameId && e.ProjectId == projectTwo.Id && projectTwo.Uuid == *e.ProjectUuid
			}), -1)
		}
	})

	t.Run("can filter environments in the whole organization by env_type id", func(t *testing.T) {
		res, err := client.ListEnvironmentsInOrgWithResponse(t.Context(), orgId, &genclient.ListEnvironmentsInOrgParams{ByEnvTypeId: &[]string{"not-existing-env-type"}})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Empty(t, res.JSON200.Items)
		}

		res, err = client.ListEnvironmentsInOrgWithResponse(t.Context(), orgId, &genclient.ListEnvironmentsInOrgParams{ByEnvTypeId: &[]string{envType}})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 3)
		}
	})

	t.Run("cannot get a non-existing environment", func(t *testing.T) {
		res, err := client.GetEnvironmentWithResponse(t.Context(), orgId, projectId, "id-does-not-exist")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot delete a non-existing environment", func(t *testing.T) {
		res, err := client.DeleteEnvironmentWithResponse(t.Context(), orgId, projectId, "id-does-not-exist", &genclient.DeleteEnvironmentParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("can delete an environment into deleting mode", func(t *testing.T) {
		res, err := client.DeleteEnvironmentWithResponse(t.Context(), orgId, projectId, envDisplayNameId, &genclient.DeleteEnvironmentParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusAccepted, res.StatusCode())
			assert.Equal(t, "deleting", string(res.JSON202.Status))
			assert.NotEqual(t, res.JSON202.UpdatedAt, res.JSON202.CreatedAt)
			assert.Equal(t, "Attempting to destroy environment", *res.JSON202.StatusMessage)
		}
	})

	t.Run("eventually the environment is gone", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			res, err := client.GetEnvironmentWithResponse(t.Context(), orgId, projectId, envDisplayNameId)
			require.NoError(collect, err)
			require.Equal(collect, http.StatusNotFound, res.StatusCode())
		}, time.Minute, time.Second, "environment still exists")
	})

	// Check module rules behaviour on delete envs

	t.Run("can create the environment again for module rule tests", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: envDisplayNameId, EnvTypeId: envType, DisplayName: ref.Ref("My Awesome Env")})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
	})

	var envModuleRuleId, projectModuleRuleId uuid.UUID
	rt := MustCreateResourceType(t, client, orgId, "rt-"+strings.ToLower(rand.Text()))
	moduleId := MustCreateEmptyModule(t, client, orgId, "mod-"+strings.ToLower(rand.Text()), rt.Id).Id
	t.Run("can create a module rule for the project and the env", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			genclient.CreateModuleRuleInOrgJSONRequestBody{ModuleId: moduleId, ProjectId: &projectId, EnvId: &envDisplayNameId},
		)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, projectId, *res.JSON201.ProjectId)
			assert.Equal(t, envDisplayNameId, *res.JSON201.EnvId)
			envModuleRuleId = res.JSON201.Id
		}
	})

	t.Run("can create a module rule for the project", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			genclient.CreateModuleRuleInOrgJSONRequestBody{ModuleId: moduleId, ProjectId: &projectId},
		)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, projectId, *res.JSON201.ProjectId)
			assert.Nil(t, res.JSON201.EnvId)
			projectModuleRuleId = res.JSON201.Id
		}
	})

	// TODO: update tests with non-internal endpoint when dp can handle deleteRules in event

	t.Run("can force delete an environment with the internal endpoint - no deleteRules flag", func(t *testing.T) {
		res, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgId, projectId, envDisplayNameId, &genclient.InternalForceDeleteEnvironmentParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("the rule for the deleted environment still exists", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			envModuleRuleId,
		)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, envModuleRuleId, res.JSON200.Id)
			assert.Equal(t, moduleId, res.JSON200.ModuleId)
		}
	})

	t.Run("can create the environment again for module rule tests", func(t *testing.T) {
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, genclient.EnvironmentCreateBody{Id: envDisplayNameId, EnvTypeId: envType, DisplayName: ref.Ref("My Awesome Env")})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("can force delete an environment with the internal endpoint - deleteRules flag", func(t *testing.T) {
		res, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgId, projectId, envDisplayNameId, &genclient.InternalForceDeleteEnvironmentParams{DeleteRules: ref.Ref(true)})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("the rule for the deleted environment does not exist anymore", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			envModuleRuleId,
		)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode())
		}
	})

	t.Run("the rule for the project still exists", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			projectModuleRuleId,
		)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, projectModuleRuleId, res.JSON200.Id)
			assert.Equal(t, moduleId, res.JSON200.ModuleId)
		}
	})

}
