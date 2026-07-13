package integrationtests

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genclient"
)

func TestModuleProvidersCrud(t *testing.T) {
	t.Parallel()
	client := MustServerClient(t)
	orgId := MustCreateOrg(t, MustInternalServerClient(t)).Id

	t.Run("no providers before create", func(t *testing.T) {
		res, err := client.ListModuleProvidersWithResponse(t.Context(), orgId, &genclient.ListModuleProvidersParams{})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Empty(t, res.JSON200.Items)
		}

		res, err = client.ListModuleProvidersWithResponse(t.Context(), orgId, &genclient.ListModuleProvidersParams{ByProviderType: ref.Ref("something")})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res.StatusCode()) {
			assert.Empty(t, res.JSON200.NextPageToken)
			assert.Empty(t, res.JSON200.Items)
		}

		res2, err := client.GetModuleProviderWithResponse(t.Context(), orgId, "thing", "id")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res2.StatusCode())
		}

		res3, err := client.UpdateModuleProviderWithResponse(t.Context(), orgId, "thing", "id", genclient.UpdateModuleProviderJSONRequestBody{})
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res3.StatusCode())
		}

		res4, err := client.DeleteModuleProviderWithResponse(t.Context(), orgId, "thing", "id")
		if assert.NoError(t, err) {
			assert.Equal(t, http.StatusNotFound, res4.StatusCode())
		}
	})

	t.Run("cannot create with invalid inputs", func(t *testing.T) {
		for i, tc := range []struct {
			body        genclient.CreateModuleProviderJSONRequestBody
			expectedErr string
		}{
			{expectedErr: "id must be a valid OpenTofu identifier"},
			{body: genclient.CreateModuleProviderJSONRequestBody{Id: "my-default-123"}, expectedErr: "provider_type must be a valid OpenTofu identifier"},
			{body: genclient.CreateModuleProviderJSONRequestBody{ProviderType: "my-type", Id: "my-default-123"}, expectedErr: "source must match (hostname/)namespace/type"},
			{body: genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws",
			}, expectedErr: "version_constraint must be a valid provider version constraint"},
			{body: genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
				Description: ref.Ref(strings.Repeat("a", 1000)),
			}, expectedErr: "maximum string length is 200"},
			{body: genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
				Configuration: map[string]interface{}{"a": ref.Ref(strings.Repeat("a", 15*1024+1))},
			}, expectedErr: "provider configuration is too large"},
			{body: genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
				Configuration: map[string]interface{}{"a": "${a}"},
			}, expectedErr: "invalid configuration: a: invalid placeholder 'a': @0: expected one of context., self.outputs., var., resources., shared., or select.; got \"a\""},
			{body: genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
				Configuration: map[string]interface{}{"a": "${context.animal}"},
			}, expectedErr: "invalid configuration: 'animal' is not a supported context placeholder key"},
		} {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				res, err := client.CreateModuleProviderWithResponse(t.Context(), orgId, tc.body)
				if assert.NoError(t, err) && assert.Equal(t, http.StatusBadRequest, res.StatusCode()) {
					assert.Contains(t, res.JSON400.Message, tc.expectedErr)
				}
			})
		}
	})

	t.Run("cannot create in org that doesn't exist", func(t *testing.T) {
		res, err := client.CreateModuleProviderWithResponse(t.Context(), orgId+orgId, genclient.CreateModuleProviderJSONRequestBody{
			ProviderType: "my-type", Id: "my-default-123", Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusConflict, res.StatusCode(), string(res.Body)) {
			assert.Equal(t, "organization not found", res.JSON409.Message)
		}
	})

	providerId := "prov-" + strings.ToLower(rand.Text())
	t.Run("can create with simple valid inputs", func(t *testing.T) {
		res, err := client.CreateModuleProviderWithResponse(t.Context(), orgId, genclient.CreateModuleProviderJSONRequestBody{
			ProviderType: "my-type", Id: providerId, Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
			Description: ref.Ref("my aws provider"),
			Configuration: map[string]interface{}{
				"region":      "us-east-1",
				"ctx-example": "${context.env_id}",
				"var-example": "${var.SOMETHING}",
				"res-example": "${resources.foo.outputs.thing}",
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode()) {
			assert.Equal(t, orgId, res.JSON201.OrgId)
			assert.Equal(t, "my-type", res.JSON201.ProviderType)
			assert.Equal(t, providerId, res.JSON201.Id)
			assert.Equal(t, "registry.terraform.io/hashicorp/aws", res.JSON201.Source)
			assert.Equal(t, "~> 5.0", res.JSON201.VersionConstraint)
			assert.Equal(t, "my aws provider", *res.JSON201.Description)
			assert.Equal(t, map[string]interface{}{
				"region":      "us-east-1",
				"ctx-example": "${context.env_id}",
				"var-example": "${var.SOMETHING}",
				"res-example": "${resources.foo.outputs.thing}",
			}, res.JSON201.Configuration)
		}

		t.Run("can get new provider", func(t *testing.T) {
			res2, err := client.GetModuleProviderWithResponse(t.Context(), orgId, res.JSON201.ProviderType, res.JSON201.Id)
			if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res2.StatusCode()) {
				assert.Equal(t, *res.JSON201, *res2.JSON200)
			}
		})

		t.Run("can list provider", func(t *testing.T) {
			res2, err := client.ListModuleProvidersWithResponse(t.Context(), orgId, &genclient.ListModuleProvidersParams{})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res2.StatusCode()) && assert.Len(t, res2.JSON200.Items, 1) {
				assert.Empty(t, res2.JSON200.NextPageToken)
				assert.Equal(t, genclient.ModuleProviderSummary{
					CreatedAt:    res.JSON201.CreatedAt,
					Description:  res.JSON201.Description,
					Id:           res.JSON201.Id,
					OrgId:        orgId,
					ProviderType: res.JSON201.ProviderType,
					Source:       res.JSON201.Source,
				}, res2.JSON200.Items[0])
			}
		})

		t.Run("cannot use the same name again", func(t *testing.T) {
			res2, err := client.CreateModuleProviderWithResponse(t.Context(), orgId, genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: "my-type", Id: providerId, Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
			})
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusConflict, res2.StatusCode())
			}
		})

		t.Run("can update provider", func(t *testing.T) {
			// no update
			res2, err := client.UpdateModuleProviderWithResponse(t.Context(), orgId, res.JSON201.ProviderType, res.JSON201.Id, genclient.ModuleProviderUpdateBody{})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res2.StatusCode()) {
				assert.Equal(t, *res.JSON201, *res2.JSON200)
			}
			// remove the description, adjust the constraint, and clear the config
			res2, err = client.UpdateModuleProviderWithResponse(t.Context(), orgId, res.JSON201.ProviderType, res.JSON201.Id, genclient.ModuleProviderUpdateBody{
				Description:       ref.Ref(""),
				VersionConstraint: ref.Ref("~> 4.0"),
				Configuration:     &map[string]interface{}{},
			})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, res2.StatusCode()) {
				assert.Equal(t, "~> 4.0", res2.JSON200.VersionConstraint)
				assert.Empty(t, res2.JSON200.Description)
				assert.Equal(t, map[string]interface{}{}, res2.JSON200.Configuration)
			}
		})

		t.Run("can delete provider", func(t *testing.T) {
			res2, err := client.DeleteModuleProviderWithResponse(t.Context(), orgId, res.JSON201.ProviderType, res.JSON201.Id)
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusNoContent, res2.StatusCode())
				res3, err := client.GetModuleProviderWithResponse(t.Context(), orgId, res.JSON201.ProviderType, res.JSON201.Id)
				if assert.NoError(t, err) {
					assert.Equal(t, http.StatusNotFound, res3.StatusCode())
				}
			}
		})
	})

	t.Run("can paginate list", func(t *testing.T) {
		for i := range 20 {
			res, err := client.CreateModuleProviderWithResponse(t.Context(), orgId, genclient.CreateModuleProviderJSONRequestBody{
				ProviderType: fmt.Sprintf("my-type-%d", i%2+1), Id: fmt.Sprintf("my-provider-%d", i), Source: "registry.terraform.io/hashicorp/aws", VersionConstraint: "~> 5.0",
			})
			if assert.NoError(t, err) {
				assert.Equal(t, http.StatusCreated, res.StatusCode())
			}
		}

		first, nextToken, seen := true, "", make([]string, 0)
		for first || nextToken != "" {
			first = false
			t.Log("fetching page with token", nextToken)
			res, err := client.ListModuleProvidersWithResponse(t.Context(), orgId, &genclient.ListModuleProvidersParams{ByProviderType: ref.Ref("my-type-1"), PerPage: ref.Ref(5), Page: ref.RefStringEmptyNil(nextToken)})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, res.StatusCode(), string(res.Body))
			assert.LessOrEqual(t, len(res.JSON200.Items), 5)
			for _, item := range res.JSON200.Items {
				assert.NotContains(t, seen, item.Id)
				seen = append(seen, item.Id)
			}
			nextToken = ref.DerefOr(res.JSON200.NextPageToken, "")
		}
		assert.Equal(t, []string{
			"my-provider-0",
			"my-provider-10",
			"my-provider-12",
			"my-provider-14",
			"my-provider-16",
			"my-provider-18",
			"my-provider-2",
			"my-provider-4",
			"my-provider-6",
			"my-provider-8",
		}, seen)
	})

}
