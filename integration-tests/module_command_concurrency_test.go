package integrationtests

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
)

func TestConcurrentModulePublicationReplaysOneImmutableResult(t *testing.T) {
	client := MustServerClient(t)
	org := MustCreateOrg(t, MustInternalServerClient(t)).Id
	rt := MustCreateResourceType(t, client, org, "concurrent-module")
	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), org,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: "catalogue"},
		genclient.ModuleCatalogueCreateBody{Slug: "concurrent-module", ResourceType: rt.Id})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	body := genclient.ModuleVersionPublishBody{
		SemanticVersion: "1.0.0", ModuleSource: "inline", ModuleSourceCode: ptr(`output "name" { value = "concurrent" }`),
		ModuleInputs: map[string]interface{}{}, ModuleParams: map[string]genclient.ModuleParamItem{},
		ProviderMapping: map[string]string{}, Dependencies: map[string]genclient.ModuleDependencyManifest{},
		Coprovisioned: []genclient.ModuleCoProvisionManifest{},
	}
	type result struct {
		response *genclient.PublishModuleVersionResponse
		err      error
	}
	const callers = 8
	start := make(chan struct{})
	results := make(chan result, callers)
	for range callers {
		go func() {
			<-start
			response, err := client.PublishModuleVersionWithResponse(t.Context(), org, "concurrent-module",
				&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-once"}, body)
			results <- result{response: response, err: err}
		}()
	}
	close(start)
	var first *genclient.CoreModuleVersion
	for range callers {
		result := <-results
		require.NoError(t, result.err)
		require.Equal(t, http.StatusCreated, result.response.StatusCode(), string(result.response.Body))
		require.NotNil(t, result.response.JSON201)
		if first == nil {
			first = result.response.JSON201
		}
		require.Equal(t, first.Uuid, result.response.JSON201.Uuid)
	}
	versions, err := client.ListModuleVersionsWithResponse(t.Context(), org, "concurrent-module", &genclient.ListModuleVersionsParams{})
	require.NoError(t, err)
	require.NotNil(t, versions.JSON200)
	require.Len(t, versions.JSON200.Items, 1)
	events, err := client.ListModuleVersionLifecycleEventsWithResponse(t.Context(), org, "concurrent-module", first.Uuid.String())
	require.NoError(t, err)
	require.NotNil(t, events.JSON200)
	require.Len(t, *events.JSON200, 1)
	body.SemanticVersion = "1.1.0"
	reused, err := client.PublishModuleVersionWithResponse(t.Context(), org, "concurrent-module",
		&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-once"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, reused.StatusCode(), string(reused.Body))
}
