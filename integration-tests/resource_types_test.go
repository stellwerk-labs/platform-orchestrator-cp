package integrationtests

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
)

func TestResourceTypesCrud(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgId := MustCreateOrg(t, internalClient).Id

	builtInS3 := genclient.CreateResourceTypeJSONRequestBody{
		Id:          "s3",
		Description: ref.Ref("Buildin S3 bucket"),
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type": "string",
				},
			},
		},
	}
	builtInPostgres := genclient.CreateResourceTypeJSONRequestBody{
		Id: "postgres",
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"db_name": map[string]interface{}{
					"type": "string",
				},
			},
		},
		IsDeveloperAccessible: ref.Ref(false),
	}
	usersS3 := genclient.CreateResourceTypeJSONRequestBody{
		Id:          "s3",
		Description: ref.Ref("My S3 bucket"),
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"arn": map[string]interface{}{
					"type": "string",
				},
			},
		},
	}
	usersNewType := genclient.CreateResourceTypeJSONRequestBody{
		Id:          "my-type",
		Description: ref.Ref("My New Type"),
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"property": map[string]interface{}{
					"type": "string",
				},
			},
		},
	}

	t.Run("no types in the list", func(t *testing.T) {
		res, err := client.ListResourceTypesWithResponse(t.Context(), orgId, &genclient.ListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Empty(t, res.JSON200.Items)
		}

		intRes, err := internalClient.InternalListResourceTypesWithResponse(t.Context(), &genclient.InternalListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, intRes.StatusCode()) {
			assert.Empty(t, intRes.JSON200.NextPageToken)
			assert.Empty(t, intRes.JSON200.Items)
		}
	})

	t.Run("no type found", func(t *testing.T) {
		res, err := client.GetResourceTypeWithResponse(t.Context(), orgId, "s3")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode())
			assert.Equal(t, &genclient.Error{Error: "HTTP-404", Message: "resource type not found"}, res.JSON404)
		}
	})

	t.Run("can't create built-in type with invalid inputs", func(t *testing.T) {
		for i, test := range []struct {
			body        genclient.CreateResourceTypeJSONRequestBody
			expectedErr string
		}{
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "s3"},
				expectedErr: `"/output_schema": Value is not nullable`,
			},
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{OutputSchema: map[string]interface{}{}},
				expectedErr: "id must be a valid resource type identifier",
			},
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "prefix/s3", OutputSchema: map[string]interface{}{}},
				expectedErr: "id must be a valid resource type identifier",
			},
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "prefix/s3", Description: ref.Ref("Lorem ipsum dolor sit amet, consectetur adipiscing elit. Maecenas in elit arcu. In semper in diam eu commodo. Proin accumsan pharetra libero non maximus. Suspendisse porta ut purus ut imperdiet. Suspendisse elementum lorem nec odio tempus consectetur."), OutputSchema: map[string]interface{}{}},
				expectedErr: "maximum string length is 200",
			},
		} {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				res, err := internalClient.InternalCreateResourceTypeWithResponse(t.Context(), test.body)
				if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode()) {
					assert.Contains(t, res.JSON400.Message, test.expectedErr)
				}
			})
		}
	})

	t.Run("create built-in types", func(t *testing.T) {
		res, err := internalClient.InternalCreateResourceTypeWithResponse(t.Context(), builtInS3)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode()) {
			assert.Equal(t, builtInS3.Id, res.JSON201.Id)
			assert.Equal(t, builtInS3.Description, res.JSON201.Description)
			assert.Equal(t, builtInS3.OutputSchema, res.JSON201.OutputSchema)
			assert.True(t, res.JSON201.BuiltIn)
			assert.True(t, res.JSON201.IsDeveloperAccessible)
		}
		res, err = internalClient.InternalCreateResourceTypeWithResponse(t.Context(), builtInPostgres)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusCreated, res.StatusCode())
			assert.False(t, res.JSON201.IsDeveloperAccessible)
		}
	})

	t.Run("can't create existing built-in type", func(t *testing.T) {
		res, err := internalClient.InternalCreateResourceTypeWithResponse(t.Context(), genclient.CreateResourceTypeJSONRequestBody{
			Id:           "s3",
			OutputSchema: map[string]interface{}{},
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
		}
	})

	t.Run("built-in type definitions are immutable", func(t *testing.T) {
		patch := genclient.ResourceTypeUpdateBody{
			OutputSchema: &map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"db_name": map[string]interface{}{
						"type": "string",
					},
					"db_host": map[string]interface{}{
						"type": "string",
					},
				},
			},
			IsDeveloperAccessible: ref.Ref(true),
		}
		res, err := internalClient.InternalUpdateResourceTypeWithResponse(t.Context(), "postgres", patch)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode()) {
			assert.Contains(t, res.JSON409.Message, "immutable")
		}
	})

	t.Run("built-in type accessibility is immutable", func(t *testing.T) {
		patch := genclient.ResourceTypeUpdateBody{
			OutputSchema: &map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string",
					},
					"region": map[string]interface{}{
						"type": "string",
					},
				},
			},
			IsDeveloperAccessible: ref.Ref(true),
		}
		res, err := internalClient.InternalUpdateResourceTypeWithResponse(t.Context(), "s3", patch)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode()) {
			assert.Contains(t, res.JSON409.Message, "immutable")
		}
	})

	t.Run("only built-in types in the list", func(t *testing.T) {
		res, err := client.ListResourceTypesWithResponse(t.Context(), orgId, &genclient.ListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			if len(res.JSON200.Items) == 2 {
				// Should have alphabetical order
				postgres := res.JSON200.Items[0]
				assert.Equal(t, builtInPostgres.Id, postgres.Id)
				assert.Equal(t, builtInPostgres.Description, postgres.Description)
				assert.Equal(t, builtInPostgres.OutputSchema, postgres.OutputSchema)
				assert.True(t, postgres.BuiltIn)
				s3 := res.JSON200.Items[1]
				assert.Equal(t, builtInS3.Id, s3.Id)
				assert.Equal(t, builtInS3.Description, s3.Description)
				assert.Equal(t, builtInS3.OutputSchema, s3.OutputSchema)
				assert.True(t, s3.BuiltIn)
			}
		}
	})

	t.Run("can't create a new type with invalid input", func(t *testing.T) {
		for i, test := range []struct {
			body        genclient.CreateResourceTypeJSONRequestBody
			expectedErr string
		}{
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "s3"},
				expectedErr: `"/output_schema": Value is not nullable`,
			},
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "prefix/s3", OutputSchema: map[string]interface{}{}},
				expectedErr: "id must be a valid resource type identifier",
			},
			{
				body:        genclient.CreateResourceTypeJSONRequestBody{Id: "s3", Description: ref.Ref("Lorem ipsum dolor sit amet, consectetur adipiscing elit. Maecenas in elit arcu. In semper in diam eu commodo. Proin accumsan pharetra libero non maximus. Suspendisse porta ut purus ut imperdiet. Suspendisse elementum lorem nec odio tempus consectetur."), OutputSchema: map[string]interface{}{}},
				expectedErr: "maximum string length is 200",
			},
		} {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, test.body)
				if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode()) {
					assert.Contains(t, res.JSON400.Message, test.expectedErr)
				}
			})
		}
	})

	t.Run("create a new type", func(t *testing.T) {
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, usersNewType)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode()) {
			assert.Equal(t, usersNewType.Id, res.JSON201.Id)
			assert.Equal(t, usersNewType.Description, res.JSON201.Description)
			assert.Equal(t, usersNewType.OutputSchema, res.JSON201.OutputSchema)
			assert.True(t, res.JSON201.IsDeveloperAccessible)
			assert.False(t, res.JSON201.BuiltIn)
		}
	})

	t.Run("create a new type that overrides built-in type", func(t *testing.T) {
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, usersS3)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode()) {
			assert.Equal(t, usersS3.Id, res.JSON201.Id)
			assert.Equal(t, usersS3.Description, res.JSON201.Description)
			assert.Equal(t, usersS3.OutputSchema, res.JSON201.OutputSchema)
			assert.False(t, res.JSON201.BuiltIn)
			assert.True(t, res.JSON201.IsDeveloperAccessible)
		}
	})

	t.Run("can't create existing type", func(t *testing.T) {
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
			Id:           "s3",
			OutputSchema: map[string]interface{}{},
		})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
		}
	})

	t.Run("the list contains new and overridden types", func(t *testing.T) {
		res, err := client.ListResourceTypesWithResponse(t.Context(), orgId, &genclient.ListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			if len(res.JSON200.Items) == 3 {
				// Should have alphabetical order
				newType := res.JSON200.Items[0]
				assert.Equal(t, usersNewType.Id, newType.Id)
				assert.Equal(t, usersNewType.Description, newType.Description)
				assert.Equal(t, usersNewType.OutputSchema, newType.OutputSchema)
				assert.False(t, newType.BuiltIn)
				postgres := res.JSON200.Items[1]
				assert.Equal(t, builtInPostgres.Id, postgres.Id)
				assert.Equal(t, builtInPostgres.Description, postgres.Description)
				assert.Equal(t, builtInPostgres.OutputSchema, postgres.OutputSchema)
				assert.True(t, postgres.BuiltIn)
				s3 := res.JSON200.Items[2]
				assert.Equal(t, usersS3.Id, s3.Id)
				assert.Equal(t, usersS3.Description, s3.Description)
				assert.Equal(t, usersS3.OutputSchema, s3.OutputSchema)
				assert.False(t, s3.BuiltIn)
			}
		}
	})

	t.Run("custom type definitions are immutable", func(t *testing.T) {
		patch := genclient.ResourceTypeUpdateBody{
			Description:           ref.Ref("My New Type Updated"),
			IsDeveloperAccessible: ref.Ref(false),
		}
		res, err := client.UpdateResourceTypeWithResponse(t.Context(), orgId, "my-type", patch)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode()) {
			assert.Contains(t, res.JSON409.Message, "immutable")
		}
	})

	t.Run("get immutable type", func(t *testing.T) {
		res, err := client.GetResourceTypeWithResponse(t.Context(), orgId, "my-type")
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Equal(t, usersNewType.Id, res.JSON200.Id)
			assert.Equal(t, usersNewType.Description, res.JSON200.Description)
			assert.Equal(t, usersNewType.OutputSchema, res.JSON200.OutputSchema)
			assert.False(t, res.JSON200.BuiltIn)
			assert.Equal(t, genclient.ResourceTypeCatalogueStatusActive, res.JSON200.CatalogueStatus)
			assert.EqualValues(t, 1, res.JSON200.ResourceVersion)
		}
	})

	t.Run("archive and unarchive a custom type", func(t *testing.T) {
		archive, err := client.ChangeResourceTypeCatalogueStatusWithResponse(
			t.Context(), orgId, "my-type",
			genclient.ChangeResourceTypeCatalogueStatusParamsCatalogueActionArchive,
			&genclient.ChangeResourceTypeCatalogueStatusParams{IdempotencyKey: "archive-my-type"},
			genclient.ModuleReasonedCommand{ExpectedResourceVersion: 1, Reason: "Retire the catalogue entry"},
		)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, archive.StatusCode()) {
			assert.Equal(t, genclient.ResourceTypeCatalogueStatusArchived, archive.JSON200.CatalogueStatus)
			assert.EqualValues(t, 2, archive.JSON200.ResourceVersion)
			assert.NotNil(t, archive.JSON200.ArchivedAt)
			assert.Equal(t, "Retire the catalogue entry", *archive.JSON200.ArchiveReason)
		}

		unarchive, err := client.ChangeResourceTypeCatalogueStatusWithResponse(
			t.Context(), orgId, "my-type",
			genclient.ChangeResourceTypeCatalogueStatusParamsCatalogueActionUnarchive,
			&genclient.ChangeResourceTypeCatalogueStatusParams{IdempotencyKey: "unarchive-my-type"},
			genclient.ModuleReasonedCommand{ExpectedResourceVersion: 2, Reason: "Return the catalogue entry"},
		)
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, unarchive.StatusCode()) {
			assert.Equal(t, genclient.ResourceTypeCatalogueStatusActive, unarchive.JSON200.CatalogueStatus)
			assert.EqualValues(t, 3, unarchive.JSON200.ResourceVersion)
			assert.Nil(t, unarchive.JSON200.ArchivedAt)
			assert.Nil(t, unarchive.JSON200.ArchiveReason)
		}
	})

	t.Run("can't update a built-in type", func(t *testing.T) {
		patch := genclient.ResourceTypeUpdateBody{
			Description: ref.Ref("PostgreSQL database custom"),
		}
		res, err := client.UpdateResourceTypeWithResponse(t.Context(), orgId, "postgres", patch)
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
		}
	})

	t.Run("can't delete a built-in type", func(t *testing.T) {
		res, err := client.DeleteResourceTypeWithResponse(t.Context(), orgId, "postgres")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res.StatusCode())
		}
	})

	t.Run("can't delete a type with retained catalogue history", func(t *testing.T) {
		res, err := client.DeleteResourceTypeWithResponse(t.Context(), orgId, "my-type")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusConflict, res.StatusCode())
			assert.Contains(t, res.JSON409.Message, "catalogue history")
		}
	})

	t.Run("delete an unused type without retained history", func(t *testing.T) {
		res, err := client.DeleteResourceTypeWithResponse(t.Context(), orgId, "s3")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("only built-in types in the list again", func(t *testing.T) {
		res, err := client.ListResourceTypesWithResponse(t.Context(), orgId, &genclient.ListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			if len(res.JSON200.Items) == 2 {
				// Should have alphabetical order
				postgres := res.JSON200.Items[0]
				assert.Equal(t, builtInPostgres.Id, postgres.Id)
				assert.Equal(t, builtInPostgres.Description, postgres.Description)
				assert.Equal(t, builtInPostgres.OutputSchema, postgres.OutputSchema)
				assert.True(t, postgres.BuiltIn)
				s3 := res.JSON200.Items[1]
				assert.Equal(t, builtInS3.Id, s3.Id)
				assert.Equal(t, builtInS3.Description, s3.Description)
				assert.Equal(t, builtInS3.OutputSchema, s3.OutputSchema)
				assert.True(t, s3.BuiltIn)
			}
		}
	})

	t.Run("delete built-in types", func(t *testing.T) {
		res, err := internalClient.InternalDeleteResourceTypeWithResponse(t.Context(), "postgres")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}

		res, err = internalClient.InternalDeleteResourceTypeWithResponse(t.Context(), "s3")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNoContent, res.StatusCode())
		}
	})

	t.Run("only the history-retaining custom type remains", func(t *testing.T) {
		res, err := client.ListResourceTypesWithResponse(t.Context(), orgId, &genclient.ListResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			if assert.Len(t, res.JSON200.Items, 1) {
				assert.Equal(t, "my-type", res.JSON200.Items[0].Id)
				assert.Equal(t, genclient.ResourceTypeCatalogueStatusActive, res.JSON200.Items[0].CatalogueStatus)
			}
		}
	})

}
