package integrationtests

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
)

func TestResourceTypeConformanceRejectsBeforePublishingOrGraduating(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	database := lifecycleSQLDatabase(t)
	MustCreateResourceType(t, client, orgID, "cache-contract")
	output := genclient.ModuleOutputSchema{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	contract := genclient.ResourceTypeModuleContract{
		"type": "object", "properties": map[string]any{
			"module_inputs": map[string]any{"type": "object", "required": []any{"region"}, "properties": map[string]any{"region": map[string]any{"enum": []any{"eu-west"}}}},
			"module_params": map[string]any{"type": "object", "required": []any{"image"}, "properties": map[string]any{"image": map[string]any{
				"type": "object", "required": []any{"type", "is_optional"}, "properties": map[string]any{"type": map[string]any{"enum": []any{"string"}}, "is_optional": map[string]any{"enum": []any{false}}},
			}}},
			"provider_mapping": map[string]any{"type": "object", "maxProperties": 0},
			"dependencies":     map[string]any{"type": "object", "required": []any{"cache"}},
			"coprovisioned":    map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "maxItems": 0},
		},
	}
	createdType, err := client.CreateResourceTypeWithResponse(t.Context(), orgID,
		genclient.ResourceTypeCreateBody{Id: "conformant-service", OutputSchema: output, ModuleContract: &contract})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createdType.StatusCode(), string(createdType.Body))
	expectedContract, err := json.Marshal(contract)
	require.NoError(t, err)
	actualContract, err := json.Marshal(createdType.JSON201.ModuleContract)
	require.NoError(t, err)
	require.JSONEq(t, string(expectedContract), string(actualContract))
	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: uuid.NewString()},
		genclient.ModuleCatalogueCreateBody{Slug: "conformant-service", ResourceType: "conformant-service"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	valid := func(version string) genclient.ModuleVersionPublishBody {
		body := lifecyclePublication(version)
		body.OutputSchema = &output
		body.ModuleInputs = map[string]any{"region": "eu-west"}
		body.ModuleParams = map[string]genclient.ModuleParamItem{"image": {Type: genclient.String}}
		body.Dependencies = map[string]genclient.ModuleDependencyManifest{"cache": {Type: "cache-contract"}}
		return body
	}
	before := lifecycleDatabaseSnapshot(t, database, orgID)
	for _, test := range []struct {
		name, code string
		change     func(*genclient.ModuleVersionPublishBody)
	}{
		{"required-input", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) { delete(b.ModuleInputs, "region") }},
		{"input-value", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) { b.ModuleInputs["region"] = "secret-value-must-not-leak" }},
		{"parameter-type", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) {
			b.ModuleParams["image"] = genclient.ModuleParamItem{Type: genclient.Number}
		}},
		{"provider-contract", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) { b.ProviderMapping["cloud"] = "cloud.private" }},
		{"dependency-contract", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) { delete(b.Dependencies, "cache") }},
		{"coprovision-contract", "module_resource_type_nonconformant", func(b *genclient.ModuleVersionPublishBody) {
			b.Coprovisioned = []genclient.ModuleCoProvisionManifest{{Type: "cache-contract"}}
		}},
		{"missing-output", "module_output_declaration_required", func(b *genclient.ModuleVersionPublishBody) { b.OutputSchema = nil }},
		{"wrong-output", "module_output_contract_mismatch", func(b *genclient.ModuleVersionPublishBody) { b.OutputSchema = ptr(genclient.ModuleOutputSchema{}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := valid("1.0.0-rc.1")
			test.change(&body)
			response, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, "conformant-service",
				&genclient.PublishModuleVersionParams{IdempotencyKey: "conformance-retry"}, body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, response.StatusCode(), string(response.Body))
			require.Equal(t, test.code, response.JSON400.Error)
			require.NotContains(t, string(response.Body), "secret-value-must-not-leak")
			require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))
		})
	}
	validBody := valid("1.0.0-rc.1")
	proposed, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, "conformant-service",
		&genclient.PublishModuleVersionParams{IdempotencyKey: "conformance-retry"}, validBody)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, proposed.StatusCode(), string(proposed.Body))
	before = lifecycleDatabaseSnapshot(t, database, orgID)
	successor := genclient.StableModuleVersionSuccessorBody{ExpectedPrereleaseResourceVersion: proposed.JSON201.ResourceVersion,
		Reason: "Graduate the reviewed declaration", Version: valid("1.0.0")}
	successor.Version.OutputSchema = nil
	rejected, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, "conformant-service", proposed.JSON201.Uuid.String(),
		&genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "graduate-conformant"}, successor)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, rejected.StatusCode(), string(rejected.Body))
	require.Equal(t, "module_output_declaration_required", rejected.JSON400.Error)
	require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))
	successor.Version.OutputSchema = &output
	graduated, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, "conformant-service", proposed.JSON201.Uuid.String(),
		&genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "graduate-conformant"}, successor)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, graduated.StatusCode(), string(graduated.Body))
	detail, err := client.GetModuleVersionWithResponse(t.Context(), orgID, "conformant-service", graduated.JSON201.Stable.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, detail.StatusCode(), string(detail.Body))
	require.Equal(t, &output, detail.JSON200.Definition.OutputSchema)
}

func TestInvalidResourceTypeContractsRejectedByPublicAndInternalAPI(t *testing.T) {
	client, internal := MustServerClient(t), MustInternalServerClient(t)
	orgID := MustCreateOrg(t, internal).Id
	contract := genclient.ResourceTypeModuleContract{"type": "object", "$ref": "https://never-fetch.invalid/schema"}
	body := genclient.ResourceTypeCreateBody{Id: "invalid-contract", OutputSchema: map[string]any{}, ModuleContract: &contract}
	public, err := client.CreateResourceTypeWithResponse(t.Context(), orgID, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, public.StatusCode(), string(public.Body))
	require.Equal(t, "resource_type_contract_invalid", public.JSON400.Error)
	private, err := internal.InternalCreateResourceTypeWithResponse(t.Context(), body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, private.StatusCode(), string(private.Body))
	require.Equal(t, public.JSON400, private.JSON400)
	absent, err := client.GetResourceTypeWithResponse(t.Context(), orgID, body.Id)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, absent.StatusCode())
}

func TestUsedBuiltInResourceTypeCannotBeShadowedOrDeleted(t *testing.T) {
	client, internal := MustServerClient(t), MustInternalServerClient(t)
	orgID := MustCreateOrg(t, internal).Id
	typeID := "bound-" + uuid.NewString()
	body := genclient.ResourceTypeCreateBody{Id: typeID, OutputSchema: map[string]any{}}
	created, err := internal.InternalCreateResourceTypeWithResponse(t.Context(), body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	module, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: uuid.NewString()},
		genclient.ModuleCatalogueCreateBody{Slug: "bound-contract", ResourceType: typeID})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, module.StatusCode(), string(module.Body))
	shadow, err := client.CreateResourceTypeWithResponse(t.Context(), orgID, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, shadow.StatusCode(), string(shadow.Body))
	deleted, err := internal.InternalDeleteResourceTypeWithResponse(t.Context(), typeID)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, deleted.StatusCode(), string(deleted.Body))
	stillBound, err := client.GetResourceTypeWithResponse(t.Context(), orgID, typeID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, stillBound.StatusCode())
	require.True(t, stillBound.JSON200.BuiltIn)
	// Remove only this test's empty catalogue shell so global built-ins do not
	// contaminate unrelated inventory tests.
	removed, err := client.DeleteModuleWithResponse(t.Context(), orgID, "bound-contract")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, removed.StatusCode(), string(removed.Body))
	cleanup, err := internal.InternalDeleteResourceTypeWithResponse(t.Context(), typeID)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, cleanup.StatusCode(), string(cleanup.Body))
}

func TestOptionalDigestAndOutputDeclarationRoundTrip(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	createLifecycleModule(t, client, orgID, "optional-metadata")
	body := lifecyclePublication("1.0.0")
	body.ModuleSource = "git::https://example.invalid/module.git?ref=release-one"
	body.ModuleSourceCode = nil
	body.SourceRevision = ptr("release-one")
	first, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, "optional-metadata",
		&genclient.PublishModuleVersionParams{IdempotencyKey: "without-digest"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, first.StatusCode(), string(first.Body))
	require.Empty(t, first.JSON201.ArtifactDigest)
	require.Equal(t, genclient.ModuleVerificationStatus("unverified"), first.JSON201.VerificationStatus)
	transitionLifecycleVersion(t, client, orgID, "optional-metadata", "promote", *first.JSON201)
	body.SemanticVersion = "1.1.0"
	body.OutputSchema = ptr(genclient.ModuleOutputSchema{})
	second, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, "optional-metadata",
		&genclient.PublishModuleVersionParams{IdempotencyKey: "explicit-empty-output"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, second.StatusCode(), string(second.Body))
	comparison, err := client.CompareModuleVersionsWithResponse(t.Context(), orgID, "optional-metadata", first.JSON201.Uuid.String(), second.JSON201.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, comparison.StatusCode(), string(comparison.Body))
	require.True(t, comparison.JSON200.OutputSchemaChanged)
	require.Nil(t, comparison.JSON200.Before.OutputSchema)
	require.NotNil(t, comparison.JSON200.After.OutputSchema)
	require.Empty(t, *comparison.JSON200.After.OutputSchema)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(comparison.Body, &wire))
	require.Contains(t, wire["after"], "output_schema")
	require.NotContains(t, wire["before"], "output_schema")
}

func TestModuleConformanceMigrationPreservesHistoricalAbsence(t *testing.T) {
	database := MustDatabaser(t)
	migration, err := os.ReadFile("../internal/model/migrations/000026_resource_type_module_contract.sql")
	require.NoError(t, err)
	up, down := migrationSections(t, string(migration))
	tx, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	if columnExists(t, tx, "resource_types", "module_contract") {
		_, err = tx.ExecContext(t.Context(), down)
		require.NoError(t, err)
	}
	orgID := "contract-migration-" + uuid.NewString()
	_, err = tx.ExecContext(t.Context(), `INSERT INTO orgs(id, created_at) VALUES ($1, now());`, orgID)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO resource_types(org_id,id,description,output_schema,created_at)
		VALUES ($1,'historic-contract','', '{"type":"object"}',now())`, orgID)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SET CONSTRAINTS ALL IMMEDIATE`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), up)
	require.NoError(t, err)
	var absent bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT module_contract IS NULL FROM resource_types WHERE org_id=$1`, orgID).Scan(&absent))
	require.True(t, absent)
	_, err = tx.ExecContext(t.Context(), down)
	require.NoError(t, err)
	require.False(t, columnExists(t, tx, "resource_types", "module_contract"))
	require.False(t, columnExists(t, tx, "definition_versions", "output_schema"))
	require.NoError(t, tx.Rollback())
}
