package integrationtests

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestProjects(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id
	projectNoDisplayNameId := "project-" + strings.ToLower(rand.Text())
	projectDisplayNameId := "project-" + strings.ToLower(rand.Text())
	var projectNoDisplayName, projectWithDisplayName *genclient.Project

	t.Run("cannot create project for a non-existing organization", func(t *testing.T) {
		res, err := client.CreateProjectWithResponse(t.Context(), "test-org-id", genclient.ProjectCreateBody{Id: projectNoDisplayNameId})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("can create project - no display name", func(t *testing.T) {
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectNoDisplayNameId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			projectNoDisplayName = res.JSON201
			assert.Equal(t, projectNoDisplayNameId, projectNoDisplayName.Id)
			assert.Equal(t, projectNoDisplayNameId, projectNoDisplayName.DisplayName)
			assert.Equal(t, genclient.ProjectStatus("active"), projectNoDisplayName.Status)
			assert.NotEmpty(t, projectNoDisplayName.Uuid)
			assert.NotEmpty(t, projectNoDisplayName.CreatedAt)
			assert.NotEmpty(t, projectNoDisplayName.UpdatedAt)
		}
	})

	t.Run("can retrieve project with generated uuid", func(t *testing.T) {
		res, err := internalClient.GetInternalProjectByUuidWithResponse(t.Context(), "another-org", projectNoDisplayName.Uuid)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot retrieve a project by uuid in the wrong org", func(t *testing.T) {
		res, err := internalClient.GetInternalProjectByUuidWithResponse(t.Context(), orgId, projectNoDisplayName.Uuid)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, *projectNoDisplayName, *res.JSON200)
		}
	})

	t.Run("can update project display name", func(t *testing.T) {
		res, err := client.UpdateProjectWithResponse(t.Context(), orgId, projectNoDisplayNameId, genclient.ProjectUpdateBody{
			DisplayName: "My Project",
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, projectNoDisplayNameId, res.JSON200.Id)
			assert.Equal(t, "My Project", res.JSON200.DisplayName)
			assert.Equal(t, genclient.ProjectStatus("active"), res.JSON200.Status)
			assert.NotEmpty(t, res.JSON200.Uuid)
			assert.NotEmpty(t, res.JSON200.CreatedAt)
			assert.NotEmpty(t, res.JSON200.UpdatedAt)
			projectNoDisplayName = res.JSON200
		}
	})

	t.Run("can list project", func(t *testing.T) {
		res, err := client.ListProjectsWithResponse(t.Context(), orgId, &genclient.ListProjectsParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) && assert.Len(t, res.JSON200.Items, 1) {
			item := res.JSON200.Items[0]
			assert.Equal(t, *projectNoDisplayName, item)
		}
	})

	t.Run("cannot create project with same id", func(t *testing.T) {
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectNoDisplayName.Id})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
		}
	})

	t.Run("can create project - with display name", func(t *testing.T) {
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectDisplayNameId, DisplayName: ref.Ref("My Awesome Project")})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			projectWithDisplayName = res.JSON201
			assert.Equal(t, projectDisplayNameId, projectWithDisplayName.Id)
			assert.Equal(t, "My Awesome Project", projectWithDisplayName.DisplayName)
			assert.Equal(t, genclient.ProjectStatus("active"), projectWithDisplayName.Status)
			assert.NotEmpty(t, projectWithDisplayName.Uuid)
			assert.NotEmpty(t, projectWithDisplayName.CreatedAt)
			assert.NotEmpty(t, projectWithDisplayName.UpdatedAt)
		}
	})

	t.Run("can get project", func(t *testing.T) {
		res, err := client.GetProjectWithResponse(t.Context(), orgId, projectDisplayNameId)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, res.JSON200, projectWithDisplayName)
		}
	})

	t.Run("cannot get a non-existing project", func(t *testing.T) {
		res, err := client.GetProjectWithResponse(t.Context(), orgId, "id-does-not-exist")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot delete a non-existing project", func(t *testing.T) {
		res, err := client.DeleteProjectWithResponse(t.Context(), orgId, "id-does-not-exist", &genclient.DeleteProjectParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	var moduleRuleId uuid.UUID
	rt := MustCreateResourceType(t, client, orgId, "rt-"+strings.ToLower(rand.Text()))
	moduleId := MustCreateEmptyModule(t, client, orgId, "mod-"+strings.ToLower(rand.Text()), rt.Id).Id
	t.Run("can create a module rule for the project", func(t *testing.T) {
		res, err := client.CreateModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			genclient.CreateModuleRuleInOrgJSONRequestBody{ModuleId: moduleId, ProjectId: &projectDisplayNameId, ResourceClass: ref.Ref("default")},
		)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, projectDisplayNameId, *res.JSON201.ProjectId)
			moduleRuleId = res.JSON201.Id
		}
	})

	runnerId := MustCreateRunnerWithRule(t, client, orgId, "", projectDisplayNameId, "runner-"+strings.ToLower(rand.Text())).Id

	t.Run("can delete a project - no delete rule flag", func(t *testing.T) {
		res, err := client.DeleteProjectWithResponse(t.Context(), orgId, projectDisplayNameId, &genclient.DeleteProjectParams{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("the module rule for the deleted project still exists", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			moduleRuleId,
		)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, moduleRuleId, res.JSON200.Id)
			assert.Equal(t, moduleId, res.JSON200.ModuleId)
			assert.Equal(t, projectDisplayNameId, *res.JSON200.ProjectId)
		}
	})

	var runnerRuleId uuid.UUID
	t.Run("the runner rule for the deleted project still exist", func(t *testing.T) {
		res, err := client.ListRunnerRulesInOrgWithResponse(t.Context(), orgId, &genclient.ListRunnerRulesInOrgParams{ByProjectId: &projectDisplayNameId})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			assert.Len(t, res.JSON200.Items, 1)
			assert.Equal(t, runnerId, res.JSON200.Items[0].RunnerId)
			runnerRuleId = res.JSON200.Items[0].Id
		}
	})

	t.Run("cannot get the deleted project anymore", func(t *testing.T) {
		res, err := client.GetProjectWithResponse(t.Context(), orgId, projectDisplayNameId)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot create project with leading space in display name", func(t *testing.T) {
		projectId := "project-" + strings.ToLower(rand.Text())
		res, err := client.CreateProjectWithResponse(
			t.Context(),
			orgId,
			genclient.ProjectCreateBody{
				Id:          projectId,
				DisplayName: ref.Ref(" Leading space project"),
			},
		)

		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusBadRequest, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("create project with special character", func(t *testing.T) {
		projectId := "project-" + strings.ToLower(rand.Text())
		displayName := "Special character project 💫"
		res, err := client.CreateProjectWithResponse(
			t.Context(),
			orgId,
			genclient.ProjectCreateBody{
				Id:          projectId,
				DisplayName: ref.Ref(displayName),
			},
		)

		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, displayName, res.JSON201.DisplayName)
		}
	})

	t.Run("cannot delete the project with envs", func(t *testing.T) {
		proj := MustCreateProject(t, client, orgId, fmt.Sprintf("proj-%s", strings.ToLower(rand.Text())))
		et := MustCreateEnvType(t, client, orgId, fmt.Sprintf("et-%s", strings.ToLower(rand.Text())))
		_ = MustCreateRunnerWithRule(t, client, orgId, "", proj.Id, "runner-"+strings.ToLower(rand.Text())).Id
		env := MustCreateEnv(t, client, orgId, et.Id, proj.Id, fmt.Sprintf("env-%s", strings.ToLower(rand.Text())))
		{
			res, err := client.DeleteProjectWithResponse(t.Context(), orgId, proj.Id, &genclient.DeleteProjectParams{})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode()) {
				assert.Equal(t, "project contains environments", res.JSON409.Message)
			}
		}
		{
			res, err := internalClient.InternalForceDeleteEnvironmentWithResponse(t.Context(), orgId, proj.Id, env.Id, &genclient.InternalForceDeleteEnvironmentParams{})
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusNoContent, res.StatusCode())
			}
		}
		{
			res, err := client.DeleteProjectWithResponse(t.Context(), orgId, proj.Id, &genclient.DeleteProjectParams{})
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusNoContent, res.StatusCode())
			}
		}
	})

	t.Run("re-create project after deletion", func(t *testing.T) {
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: projectDisplayNameId, DisplayName: ref.Ref("Recreated Project")})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, projectDisplayNameId, res.JSON201.Id)
			assert.Equal(t, "Recreated Project", res.JSON201.DisplayName)
			assert.Equal(t, genclient.ProjectStatus("active"), res.JSON201.Status)
			assert.NotEmpty(t, res.JSON201.Uuid)
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			assert.NotEmpty(t, res.JSON201.UpdatedAt)
		}
	})

	t.Run("can delete the project with the delete rules flag", func(t *testing.T) {
		res, err := client.DeleteProjectWithResponse(t.Context(), orgId, projectDisplayNameId, &genclient.DeleteProjectParams{
			DeleteRules: ref.Ref(true),
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("the module rule for the deleted project is also deleted", func(t *testing.T) {
		res, err := client.GetModuleRuleInOrgWithResponse(
			t.Context(),
			orgId,
			moduleRuleId,
		)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("the runner rule for the deleted project is also deleted", func(t *testing.T) {
		res, err := client.GetRunnerRuleInOrgWithResponse(t.Context(), orgId, runnerRuleId)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

}
