package integrationtests

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestEnvTypes(t *testing.T) {
	t.Parallel()

	client := MustServerClient(t)

	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id

	t.Run("org must exist", func(t *testing.T) {
		if res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId+orgId, genclient.CreateEnvironmentTypeJSONRequestBody{
			Id: "dev",
		}); assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "organization not found", res.JSON409.Message)
		}
	})

	var et1 genclient.EnvironmentType
	t.Run("can create when orgs exists", func(t *testing.T) {
		if res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.EnvironmentTypeCreateBody{
			Id:          "dev",
			DisplayName: ref.Ref("Development"),
		}); assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, "dev", res.JSON201.Id)
			assert.Equal(t, "Development", res.JSON201.DisplayName)
			assert.NotEmpty(t, res.JSON201.Id)
			assert.NotEmpty(t, res.JSON201.Uuid)
			assert.NotEmpty(t, res.JSON201.CreatedAt)
			et1 = *res.JSON201
		}
	})

	t.Run("can update environment type display name", func(t *testing.T) {
		if res, err := client.UpdateEnvironmentTypeWithResponse(t.Context(), orgId, "dev", genclient.EnvironmentTypeUpdateBody{
			DisplayName: "New Development",
		}); assert.NoError(t, err) {
			assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			assert.Equal(t, "dev", res.JSON200.Id)
			assert.Equal(t, "New Development", res.JSON200.DisplayName)
			assert.NotEmpty(t, res.JSON200.Uuid)
			assert.NotEmpty(t, res.JSON200.CreatedAt)
			et1 = *res.JSON200
		}
	})

	t.Run("cannot duplicate name", func(t *testing.T) {
		if res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.EnvironmentTypeCreateBody{
			Id:          "dev",
			DisplayName: ref.Ref("Development"),
		}); assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "environment type already exists", res.JSON409.Message)
		}
	})

	t.Run("can get env type", func(t *testing.T) {
		if res, err := client.GetEnvironmentTypeWithResponse(t.Context(), orgId, "dev"); assert.NoError(t, err) {
			assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			assert.Equal(t, et1, *res.JSON200)
		}
	})

	var et2 genclient.EnvironmentType
	t.Run("can create another env type", func(t *testing.T) {
		if res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.EnvironmentTypeCreateBody{
			Id: "prod",
		}); assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
			assert.Equal(t, "prod", res.JSON201.Id)
			assert.Equal(t, "prod", res.JSON201.DisplayName)
			et2 = *res.JSON201
		}
	})

	t.Run("can list env types", func(t *testing.T) {
		res, err := client.ListEnvironmentTypesWithResponse(t.Context(), orgId, &genclient.ListEnvironmentTypesParams{PerPage: ref.Ref(1)})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
			if assert.Len(t, res.JSON200.Items, 1) {
				assert.Equal(t, et1, res.JSON200.Items[0])
			}
			if assert.NotEmpty(t, res.JSON200.NextPageToken, 1) {
				res, err = client.ListEnvironmentTypesWithResponse(t.Context(), orgId, &genclient.ListEnvironmentTypesParams{Page: res.JSON200.NextPageToken})
				if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body)) {
					if assert.Len(t, res.JSON200.Items, 1) {
						assert.Equal(t, et2, res.JSON200.Items[0])
					}
					assert.Empty(t, res.JSON200.NextPageToken)
				}
			}
		}
	})

	t.Run("can delete env type", func(t *testing.T) {
		res, err := client.DeleteEnvironmentTypeWithResponse(t.Context(), orgId, et1.Id)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("cannot get deleted env type", func(t *testing.T) {
		if res, err := client.GetEnvironmentTypeWithResponse(t.Context(), orgId, et1.Id); assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode(), string(res.Body))
		}
	})

	t.Run("cannot delete env type that is used", func(t *testing.T) {
		if res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.ProjectCreateBody{Id: "project-1"}); assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
		_ = MustCreateRunnerWithRule(t, client, orgId, "", "", "runner-1")
		if res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, "project-1", genclient.EnvironmentCreateBody{Id: "env-1", EnvTypeId: et2.Id}); assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		}
		res, err := client.DeleteEnvironmentTypeWithResponse(t.Context(), orgId, et2.Id)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode()) {
			assert.Equal(t, "environment type is in use by app 'project-1' env 'env-1'", res.JSON409.Message)
		}
	})

}
