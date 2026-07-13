package integrationtests

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestAvailableResourceTypesCrud(t *testing.T) {
	client := MustServerClient(t)
	internalClient := MustInternalServerClient(t)
	orgId := MustCreateOrg(t, internalClient).Id
	_ = MustCreateRunnerWithRule(t, client, orgId, "", "", "runner-"+strings.ToLower(rand.Text()))

	{
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.CreateProjectJSONRequestBody{
			Id: "my-project",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := client.CreateProjectWithResponse(t.Context(), orgId, genclient.CreateProjectJSONRequestBody{
			Id: "other-project",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.CreateEnvironmentTypeJSONRequestBody{
			Id: "dev",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := client.CreateEnvironmentTypeWithResponse(t.Context(), orgId, genclient.CreateEnvironmentTypeJSONRequestBody{
			Id: "stg",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, "my-project", genclient.CreateEnvironmentJSONRequestBody{
			EnvTypeId: "dev",
			Id:        "my-env",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := client.CreateEnvironmentWithResponse(t.Context(), orgId, "other-project", genclient.CreateEnvironmentJSONRequestBody{
			EnvTypeId: "stg",
			Id:        "my-env",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
			Id:          "s3",
			Description: ref.Ref("My S3 bucket"),
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string",
					},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := internalClient.InternalCreateResourceTypeWithResponse(t.Context(), genclient.CreateResourceTypeJSONRequestBody{
			Id:          "s3",
			Description: ref.Ref("My S3 bucket"),
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string",
					},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}
	defer func() {
		res, err := internalClient.InternalDeleteResourceTypeWithResponse(t.Context(), "s3")
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, res.StatusCode())
	}()
	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
			Id:          "k8s-cluster",
			Description: ref.Ref("My Kubernetes Cluster"),
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cluster_name": map[string]interface{}{
						"type": "string",
					},
				},
			},
			IsDeveloperAccessible: ref.Ref(false),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := client.CreateResourceTypeWithResponse(t.Context(), orgId, genclient.CreateResourceTypeJSONRequestBody{
			Id:          "postgres",
			Description: ref.Ref("My PostgreSQL database"),
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"db_name": map[string]interface{}{
						"type": "string",
					},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	{
		res, err := internalClient.InternalCreateResourceTypeWithResponse(t.Context(), genclient.CreateResourceTypeJSONRequestBody{
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
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}
	defer func() {
		res, err := internalClient.InternalDeleteResourceTypeWithResponse(t.Context(), "my-type")
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, res.StatusCode())
	}()

	const provType = "aws"
	const provId = "default"
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

	const k8sClusterModuleId = "my-k8s-cluster-definition"
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           k8sClusterModuleId,
			ResourceType: "k8s-cluster",
			ModuleSource: "/modules/my-module",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
			ModuleParams: map[string]genclient.ModuleParamItem{
				"fruit": {
					Type:        "string",
					IsOptional:  true,
					Description: ref.Ref("The fruit to use in the cluster"),
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	const s3ModuleId = "my-s3-definition"
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           s3ModuleId,
			ResourceType: "s3",
			ModuleSource: "/modules/my-s3",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	const anotherS3ModuleId = "my-another-s3-definition"
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           anotherS3ModuleId,
			ResourceType: "s3",
			ModuleSource: "/modules/another-my-s3",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	const postgresModuleId = "my-postgres-definition"
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           postgresModuleId,
			ResourceType: "postgres",
			ModuleSource: "/modules/my-postgres",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	const myTypeModuleId = "my-type-definition"
	{
		res, err := client.CreateModuleWithResponse(t.Context(), orgId, genclient.CreateModuleJSONRequestBody{
			Id:           myTypeModuleId,
			ResourceType: "my-type",
			ModuleSource: "/modules/my-type",
			ProviderMapping: map[string]string{
				provType: provType + "." + provId,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
	}

	var defaultK8sClusterRuleId string
	{
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:  k8sClusterModuleId,
			EnvTypeId: ref.Ref("dev"),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		defaultK8sClusterRuleId = res.JSON201.Id.String()
	}

	var s3CustomClassRuleId string
	{
		resourceClass := "custom-class"
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:      s3ModuleId,
			ProjectId:     ref.Ref("my-project"),
			ResourceClass: &resourceClass,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		s3CustomClassRuleId = res.JSON201.Id.String()

	}

	var defaultS3RuleId string
	{
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:  anotherS3ModuleId,
			EnvTypeId: ref.Ref("dev"),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		defaultS3RuleId = res.JSON201.Id.String()
	}

	var otherAppS3RuleId string
	{
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:  anotherS3ModuleId,
			ProjectId: ref.Ref("other-project"),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		otherAppS3RuleId = res.JSON201.Id.String()
	}

	var postgresRuleId string
	{
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId:   postgresModuleId,
			ProjectId:  ref.Ref("my-project"),
			ResourceId: ref.Ref("my-postgres-instance"),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		postgresRuleId = res.JSON201.Id.String()
	}

	var myTypeRuleId string
	{
		res, err := client.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, genclient.RuleCreateBody{
			ModuleId: myTypeModuleId,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode())
		myTypeRuleId = res.JSON201.Id.String()
	}

	t.Run("list available resource types for my-project", func(t *testing.T) {
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "my-project", "my-env", &genclient.ListAvailableResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Len(t, res.JSON200.Items, 3, "Expected 3 resource types, got %d", len(res.JSON200.Items))

			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
				Id:          "s3",
				Description: ref.Ref("My S3 bucket"),
				OutputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type": "string",
						},
					},
				},
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      s3ModuleId,
						ResourceClass: "custom-class",
						RuleId:        s3CustomClassRuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
					{
						ModuleId:      anotherS3ModuleId,
						ResourceClass: "default",
						RuleId:        defaultS3RuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})

			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
				Id:          "postgres",
				Description: ref.Ref("My PostgreSQL database"),
				OutputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"db_name": map[string]interface{}{
							"type": "string",
						},
					},
				},
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      postgresModuleId,
						ResourceClass: "default",
						ResourceId:    ref.Ref("my-postgres-instance"),
						RuleId:        postgresRuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})

			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
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
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      myTypeModuleId,
						ResourceClass: "default",
						RuleId:        myTypeRuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})
		}
	})

	t.Run("list available resource types for my-project - filtering by type", func(t *testing.T) {
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "my-project", "my-env", &genclient.ListAvailableResourceTypesParams{
			TypeId: ref.Ref("s3"),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Len(t, res.JSON200.Items, 1, "Expected 1 resource type, got %d", len(res.JSON200.Items))

			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
				Id:          "s3",
				Description: ref.Ref("My S3 bucket"),
				OutputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type": "string",
						},
					},
				},
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      s3ModuleId,
						ResourceClass: "custom-class",
						RuleId:        s3CustomClassRuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
					{
						ModuleId:      anotherS3ModuleId,
						ResourceClass: "default",
						RuleId:        defaultS3RuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})
		}
	})

	t.Run("list available resource types for my-project - filtering by type with including no developer accessible types", func(t *testing.T) {
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "my-project", "my-env", &genclient.ListAvailableResourceTypesParams{
			TypeId:                        ref.Ref("k8s-cluster"),
			IncludeNonDeveloperAccessible: ref.Ref(true),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Len(t, res.JSON200.Items, 1, "Expected 1 resource type, got %d", len(res.JSON200.Items))

			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
				Id:          "k8s-cluster",
				Description: ref.Ref("My Kubernetes Cluster"),
				OutputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"cluster_name": map[string]interface{}{
							"type": "string",
						},
					},
				},
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      k8sClusterModuleId,
						ResourceClass: "default",
						RuleId:        defaultK8sClusterRuleId,
						ModuleParams: map[string]genclient.ModuleParamItem{
							"fruit": {
								Type:        "string",
								IsOptional:  true,
								Description: ref.Ref("The fruit to use in the cluster"),
							},
						},
					},
				},
			})
		}
	})

	t.Run("list available resource types for other-project", func(t *testing.T) {
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "other-project", "my-env", &genclient.ListAvailableResourceTypesParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Len(t, res.JSON200.Items, 2, "Expected 2 resource type, got %d", len(res.JSON200.Items))
			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
				Id:          "s3",
				Description: ref.Ref("My S3 bucket"),
				OutputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type": "string",
						},
					},
				},
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      anotherS3ModuleId,
						ResourceClass: "default",
						RuleId:        otherAppS3RuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})
			assert.Contains(t, res.JSON200.Items, genclient.AvailableResourceType{
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
				Options: []genclient.AvailableResourceTypeOption{
					{
						ModuleId:      myTypeModuleId,
						ResourceClass: "default",
						RuleId:        myTypeRuleId,
						ModuleParams:  map[string]genclient.ModuleParamItem{},
					},
				},
			})
		}
	})

	t.Run("list available resource types for other-project - empty response", func(t *testing.T) {
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "other-project", "my-env", &genclient.ListAvailableResourceTypesParams{
			TypeId: ref.Ref("postgres"),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Empty(t, res.JSON200.Items)
		}
	})

	t.Run("available resource resource types paginate correctly", func(t *testing.T) {
		perPage := 2
		res, err := client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "my-project", "my-env", &genclient.ListAvailableResourceTypesParams{
			PerPage: &perPage,
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Len(t, res.JSON200.Items, 2)
			assert.NotEmpty(t, res.JSON200.NextPageToken)
		}
		res, err = client.ListAvailableResourceTypesWithResponse(t.Context(), orgId, "my-project", "my-env", &genclient.ListAvailableResourceTypesParams{
			Page:    res.JSON200.NextPageToken,
			PerPage: &perPage,
		})
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.StatusCode())
	})
}
