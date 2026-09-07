package integrationtests

import (
	"database/sql"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func lifecyclePublication(version string) genclient.ModuleVersionPublishBody {
	return genclient.ModuleVersionPublishBody{
		SemanticVersion: version, ModuleSource: "inline", ModuleSourceCode: ptr(`output "name" { value = "atomic-release" }`),
		ModuleInputs: map[string]interface{}{}, ModuleParams: map[string]genclient.ModuleParamItem{},
		ProviderMapping: map[string]string{}, Dependencies: map[string]genclient.ModuleDependencyManifest{},
		Coprovisioned: []genclient.ModuleCoProvisionManifest{},
	}
}

func createLifecycleModule(t *testing.T, client genclient.ClientWithResponsesInterface, orgID, moduleID string) {
	t.Helper()
	rt := MustCreateResourceType(t, client, orgID, moduleID)
	created, err := client.CreateModuleCatalogueEntryWithResponse(t.Context(), orgID,
		&genclient.CreateModuleCatalogueEntryParams{IdempotencyKey: uuid.NewString()},
		genclient.ModuleCatalogueCreateBody{Slug: moduleID, ResourceType: rt.Id})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
}

func publishLifecycleVersion(t *testing.T, client genclient.ClientWithResponsesInterface, orgID, moduleID, version string) genclient.CoreModuleVersion {
	t.Helper()
	created, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleID,
		&genclient.PublishModuleVersionParams{IdempotencyKey: uuid.NewString()}, lifecyclePublication(version))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, created.StatusCode(), string(created.Body))
	require.NotNil(t, created.JSON201)
	return *created.JSON201
}

func transitionLifecycleVersion(t *testing.T, client genclient.ClientWithResponsesInterface, orgID, moduleID, action string, version genclient.CoreModuleVersion) genclient.CoreModuleVersion {
	t.Helper()
	changed, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleID, version.Uuid.String(),
		genclient.TransitionModuleVersionParamsLifecycleAction(action), &genclient.TransitionModuleVersionParams{IdempotencyKey: uuid.NewString()},
		genclient.ModuleReasonedCommand{ExpectedResourceVersion: version.ResourceVersion, Reason: "Prepare atomic lifecycle acceptance"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, changed.StatusCode(), string(changed.Body))
	require.NotNil(t, changed.JSON200)
	return *changed.JSON200
}

// Read all persisted domain rows for the isolated org. Comparing complete rows
// detects partial pointer, definition, lifecycle-event and command-receipt writes.
// Outbox transport rows are intentionally excluded because delivery is async.
func lifecycleDatabaseSnapshot(t *testing.T, database *sql.DB, orgID string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT jsonb_build_object(
		'modules', (SELECT coalesce(jsonb_agg(to_jsonb(d) ORDER BY d.id), '[]'::jsonb) FROM definitions d WHERE d.org_id=$1),
		'versions', (SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY v.uuid), '[]'::jsonb) FROM definition_versions v WHERE v.org_id=$1),
		'events', (SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.sequence), '[]'::jsonb) FROM module_version_lifecycle_events e WHERE e.org_id=$1),
		'commands', (SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.command_scope,c.idempotency_key), '[]'::jsonb) FROM module_core_commands c WHERE c.org_id=$1)
	)`, orgID).Scan(&snapshot))
	return snapshot
}

func lifecycleSQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	connection := os.Getenv("DB_CONNECTION_STRING")
	require.NotEmpty(t, connection, "DB_CONNECTION_STRING must identify the isolated integration database")
	database, err := sql.Open("postgres", connection)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

func TestStableGraduationFailuresLeavePrereleaseAndHistoryUnchanged(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	database := lifecycleSQLDatabase(t)
	const moduleID = "stable-atomicity"
	createLifecycleModule(t, client, orgID, moduleID)
	first := publishLifecycleVersion(t, client, orgID, moduleID, "1.0.0")
	transitionLifecycleVersion(t, client, orgID, moduleID, "promote", first)
	reserved := publishLifecycleVersion(t, client, orgID, moduleID, "2.0.0")
	transitionLifecycleVersion(t, client, orgID, moduleID, "deprecate", reserved)
	prerelease := publishLifecycleVersion(t, client, orgID, moduleID, "1.1.0-rc.1")
	before := lifecycleDatabaseSnapshot(t, database, orgID)
	body := genclient.StableModuleVersionSuccessorBody{
		ExpectedPrereleaseResourceVersion: prerelease.ResourceVersion,
		Reason:                            "Graduate the reviewed release candidate", Version: lifecyclePublication("1.1.0"),
	}
	for _, test := range []struct {
		name   string
		status int
		change func(*genclient.StableModuleVersionSuccessorBody)
	}{
		{"incomplete-definition", http.StatusBadRequest, func(b *genclient.StableModuleVersionSuccessorBody) { b.Version.ModuleSourceCode = nil }},
		{"blank-reason", http.StatusBadRequest, func(b *genclient.StableModuleVersionSuccessorBody) { b.Reason = " " }},
		{"prerelease-successor", http.StatusBadRequest, func(b *genclient.StableModuleVersionSuccessorBody) { b.Version.SemanticVersion = "1.1.0-rc.2" }},
		{"stale-prerelease-revision", http.StatusConflict, func(b *genclient.StableModuleVersionSuccessorBody) { b.ExpectedPrereleaseResourceVersion++ }},
		{"missing-contract", http.StatusConflict, func(b *genclient.StableModuleVersionSuccessorBody) {
			b.Version.Dependencies = map[string]genclient.ModuleDependencyManifest{"database": {Type: "missing-contract"}}
		}},
		// The unique SemVer constraint fails after the model has attempted to
		// deprecate the prerelease. The entire SQL transaction must roll back.
		{"reserved-semver-after-deprecation", http.StatusConflict, func(b *genclient.StableModuleVersionSuccessorBody) { b.Version.SemanticVersion = "2.0.0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := body
			test.change(&invalid)
			response, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleID, prerelease.Uuid.String(),
				&genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: test.name}, invalid)
			require.NoError(t, err)
			require.Equal(t, test.status, response.StatusCode(), string(response.Body))
			require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID), "failed graduation must not leave partial domain writes")
		})
	}
	unauthorized := MustServerClientWithId(t, userid.NewHumanUserId().String())
	denied, err := unauthorized.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleID, prerelease.Uuid.String(),
		&genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "unauthorized"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, denied.StatusCode(), string(denied.Body))
	require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))

	// A rejected transaction stored no receipt, so correcting its input under
	// that key must succeed. After success, an exact replay is read-only.
	params := &genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "reserved-semver-after-deprecation"}
	graduated, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleID, prerelease.Uuid.String(), params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, graduated.StatusCode(), string(graduated.Body))
	require.NotNil(t, graduated.JSON201)
	require.Equal(t, genclient.ModuleVersionSemanticStatus("deprecated"), graduated.JSON201.Prerelease.LifecycleStatus)
	require.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), graduated.JSON201.Stable.LifecycleStatus)
	require.NotEqual(t, prerelease.Uuid, graduated.JSON201.Stable.Uuid)
	var correlatedEvents int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM module_version_lifecycle_events WHERE org_id=$1 AND correlation_id=$2`, orgID, graduated.JSON201.CorrelationId).Scan(&correlatedEvents))
	require.Equal(t, 2, correlatedEvents)
	after := lifecycleDatabaseSnapshot(t, database, orgID)
	replayed, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleID, prerelease.Uuid.String(), params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, replayed.StatusCode(), string(replayed.Body))
	require.Equal(t, graduated.JSON201, replayed.JSON201)
	require.JSONEq(t, after, lifecycleDatabaseSnapshot(t, database, orgID))
}

func TestMultiModuleLifecycleTransactionRollsBackPartialChangesAndStaleCommands(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	database := lifecycleSQLDatabase(t)
	createLifecycleModule(t, client, orgID, "alpha")
	createLifecycleModule(t, client, orgID, "zulu")
	oldDefault := publishLifecycleVersion(t, client, orgID, "alpha", "1.0.0")
	oldDefault = transitionLifecycleVersion(t, client, orgID, "alpha", "promote", oldDefault)
	alpha := publishLifecycleVersion(t, client, orgID, "alpha", "2.0.0")
	zulu := publishLifecycleVersion(t, client, orgID, "zulu", "1.0.0")
	body := genclient.ModuleVersionLifecycleTransactionBody{Transitions: []genclient.ModuleVersionLifecycleTransactionItem{
		{ModuleId: "alpha", ModuleVersionId: alpha.Uuid.String(), ExpectedResourceVersion: alpha.ResourceVersion, Action: "promote", Reason: "Atomic fleet release"},
		{ModuleId: "zulu", ModuleVersionId: zulu.Uuid.String(), ExpectedResourceVersion: zulu.ResourceVersion + 1, Action: "promote", Reason: "Atomic fleet release"},
	}}
	before := lifecycleDatabaseSnapshot(t, database, orgID)
	rejected, err := client.TransactModuleVersionLifecyclesWithResponse(t.Context(), orgID,
		&genclient.TransactModuleVersionLifecyclesParams{IdempotencyKey: "stale-second-module"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, rejected.StatusCode(), string(rejected.Body))
	require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID), "the second Module's conflict must undo the first Module's transition and events")

	body.Transitions[1].ExpectedResourceVersion = zulu.ResourceVersion
	unauthorized := MustServerClientWithId(t, userid.NewHumanUserId().String())
	denied, err := unauthorized.TransactModuleVersionLifecyclesWithResponse(t.Context(), orgID,
		&genclient.TransactModuleVersionLifecyclesParams{IdempotencyKey: "unauthorized"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, denied.StatusCode(), string(denied.Body))
	require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))

	params := &genclient.TransactModuleVersionLifecyclesParams{IdempotencyKey: "stale-second-module"}
	completed, err := client.TransactModuleVersionLifecyclesWithResponse(t.Context(), orgID, params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, completed.StatusCode(), string(completed.Body))
	require.NotNil(t, completed.JSON200)
	require.Len(t, completed.JSON200.Versions, 2)
	for _, version := range completed.JSON200.Versions {
		require.Equal(t, genclient.ModuleVersionSemanticStatus("default"), version.LifecycleStatus)
	}
	var correlatedEvents int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM module_version_lifecycle_events WHERE org_id=$1 AND correlation_id=$2`, orgID, completed.JSON200.CorrelationId).Scan(&correlatedEvents))
	require.Equal(t, 3, correlatedEvents, "one predecessor deprecation and two promotions share the atomic correlation")
	var defaults, proposed, correctPointers int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE semantic_status='default'), count(*) FILTER (WHERE semantic_status='proposed') FROM definition_versions WHERE org_id=$1`, orgID).Scan(&defaults, &proposed))
	require.Equal(t, 2, defaults)
	require.Zero(t, proposed)
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM definitions d JOIN definition_versions v ON v.uuid=d.current_default_version_uuid WHERE d.org_id=$1 AND v.semantic_status='default'`, orgID).Scan(&correctPointers))
	require.Equal(t, 2, correctPointers)
	previous, err := client.GetModuleVersionWithResponse(t.Context(), orgID, "alpha", oldDefault.Uuid.String())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, previous.StatusCode(), string(previous.Body))
	require.Equal(t, genclient.ModuleVersionSemanticStatus("deprecated"), previous.JSON200.Version.LifecycleStatus)
	after := lifecycleDatabaseSnapshot(t, database, orgID)
	replayed, err := client.TransactModuleVersionLifecyclesWithResponse(t.Context(), orgID, params, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, replayed.StatusCode(), string(replayed.Body))
	require.Equal(t, completed.JSON200, replayed.JSON200)
	require.JSONEq(t, after, lifecycleDatabaseSnapshot(t, database, orgID))
	stale, err := client.TransactModuleVersionLifecyclesWithResponse(t.Context(), orgID,
		&genclient.TransactModuleVersionLifecyclesParams{IdempotencyKey: "new-command-with-stale-revisions"}, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, stale.StatusCode(), string(stale.Body))
	require.JSONEq(t, after, lifecycleDatabaseSnapshot(t, database, orgID))
}
