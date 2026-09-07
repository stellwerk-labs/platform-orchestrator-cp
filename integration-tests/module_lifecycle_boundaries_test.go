package integrationtests

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"
	"github.com/stretchr/testify/require"
)

func TestDistinctConcurrentPublicationsCreateExactlyOneProposedVersion(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	database := lifecycleSQLDatabase(t)
	const moduleID = "competing-publications"
	createLifecycleModule(t, client, orgID, moduleID)
	type outcome struct {
		response *genclient.PublishModuleVersionResponse
		err      error
	}
	versions := []string{"1.0.0", "1.1.0", "2.0.0", "3.0.0"}
	start := make(chan struct{})
	results := make(chan outcome, len(versions))
	for _, version := range versions {
		go func() {
			<-start
			response, err := client.PublishModuleVersionWithResponse(t.Context(), orgID, moduleID,
				&genclient.PublishModuleVersionParams{IdempotencyKey: "publish-" + version}, lifecyclePublication(version))
			results <- outcome{response: response, err: err}
		}()
	}
	close(start)
	var accepted, rejected int
	for range versions {
		result := <-results
		require.NoError(t, result.err)
		switch result.response.StatusCode() {
		case http.StatusCreated:
			accepted++
			require.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), result.response.JSON201.LifecycleStatus)
		case http.StatusConflict:
			rejected++
		default:
			t.Fatalf("unexpected concurrent publication status %d: %s", result.response.StatusCode(), result.response.Body)
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, len(versions)-1, rejected)
	var versionCount, proposedCount, eventCount int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*), count(*) FILTER (WHERE semantic_status='proposed')
		FROM definition_versions WHERE org_id=$1`, orgID).Scan(&versionCount, &proposedCount))
	require.Equal(t, 1, versionCount)
	require.Equal(t, 1, proposedCount)
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM module_version_lifecycle_events WHERE org_id=$1`, orgID).Scan(&eventCount))
	require.Equal(t, 1, eventCount, "losing candidate commands must not publish partial history")
}

func TestDefectiveDefaultRestorationNeverSkipsTheExactDeprecatedPredecessor(t *testing.T) {
	for _, defectivePredecessor := range []bool{false, true} {
		name := "exact-deprecated-predecessor"
		if defectivePredecessor {
			name = "defective-predecessor-cannot-be-skipped"
		}
		t.Run(name, func(t *testing.T) {
			client := MustServerClient(t)
			orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
			database := lifecycleSQLDatabase(t)
			const moduleID = "restore-lineage"
			createLifecycleModule(t, client, orgID, moduleID)
			for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
				candidate := publishLifecycleVersion(t, client, orgID, moduleID, version)
				transitionLifecycleVersion(t, client, orgID, moduleID, "promote", candidate)
			}
			read := func(version string) genclient.CoreModuleVersion {
				response, err := client.GetModuleVersionWithResponse(t.Context(), orgID, moduleID, version)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode(), string(response.Body))
				return response.JSON200.Version
			}
			first, second, third := read("1.0.0"), read("2.0.0"), read("3.0.0")
			third = transitionLifecycleVersion(t, client, orgID, moduleID, "mark-defective", third)
			catalogue, err := client.GetModuleCatalogueEntryWithResponse(t.Context(), orgID, moduleID)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, catalogue.StatusCode(), string(catalogue.Body))
			require.Nil(t, catalogue.JSON200.CurrentDefaultVersionUuid)
			var defaultCount int
			require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM definition_versions WHERE org_id=$1 AND semantic_status='default'`, orgID).Scan(&defaultCount))
			require.Zero(t, defaultCount)
			if defectivePredecessor {
				second = transitionLifecycleVersion(t, client, orgID, moduleID, "mark-defective", second)
			}
			before := lifecycleDatabaseSnapshot(t, database, orgID)
			for _, invalid := range []struct {
				version genclient.CoreModuleVersion
				status  int
			}{{first, http.StatusConflict}, {third, http.StatusBadRequest}} {
				response, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleID, invalid.version.Uuid.String(),
					genclient.TransitionModuleVersionParamsLifecycleAction("restore"), &genclient.TransitionModuleVersionParams{IdempotencyKey: uuid.NewString()},
					genclient.ModuleReasonedCommand{ExpectedResourceVersion: invalid.version.ResourceVersion, Reason: "Do not bypass the previous Default"})
				require.NoError(t, err)
				require.Equal(t, invalid.status, response.StatusCode(), string(response.Body))
				require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))
			}
			if defectivePredecessor {
				response, err := client.TransitionModuleVersionWithResponse(t.Context(), orgID, moduleID, second.Uuid.String(),
					genclient.TransitionModuleVersionParamsLifecycleAction("restore"), &genclient.TransitionModuleVersionParams{IdempotencyKey: uuid.NewString()},
					genclient.ModuleReasonedCommand{ExpectedResourceVersion: second.ResourceVersion, Reason: "A Defective predecessor is not restorable"})
				require.NoError(t, err)
				require.Equal(t, http.StatusBadRequest, response.StatusCode(), string(response.Body))
				require.JSONEq(t, before, lifecycleDatabaseSnapshot(t, database, orgID))
				return
			}
			restored := transitionLifecycleVersion(t, client, orgID, moduleID, "restore", second)
			require.Equal(t, genclient.ModuleVersionSemanticStatus("default"), restored.LifecycleStatus)
			catalogue, err = client.GetModuleCatalogueEntryWithResponse(t.Context(), orgID, moduleID)
			require.NoError(t, err)
			require.Equal(t, &second.Uuid, catalogue.JSON200.CurrentDefaultVersionUuid)
			require.Equal(t, genclient.ModuleVersionSemanticStatus("deprecated"), read("1.0.0").LifecycleStatus)
			require.Equal(t, genclient.ModuleVersionSemanticStatus("defective"), read("3.0.0").LifecycleStatus)
		})
	}
}

func TestConcurrentStableSuccessorCommandsCreateOneStableProposedVersion(t *testing.T) {
	client := MustServerClient(t)
	orgID := MustCreateOrg(t, MustInternalServerClient(t)).Id
	database := lifecycleSQLDatabase(t)
	const moduleID = "concurrent-stable-successor"
	createLifecycleModule(t, client, orgID, moduleID)
	first := publishLifecycleVersion(t, client, orgID, moduleID, "1.0.0")
	transitionLifecycleVersion(t, client, orgID, moduleID, "promote", first)
	prerelease := publishLifecycleVersion(t, client, orgID, moduleID, "1.1.0-rc.1")

	type outcome struct {
		response *genclient.PublishStableModuleVersionSuccessorResponse
		err      error
	}
	stableVersions := []string{"1.1.0", "1.2.0", "2.0.0"}
	start := make(chan struct{})
	results := make(chan outcome, len(stableVersions))
	for _, version := range stableVersions {
		go func() {
			<-start
			response, err := client.PublishStableModuleVersionSuccessorWithResponse(t.Context(), orgID, moduleID, prerelease.Uuid.String(),
				&genclient.PublishStableModuleVersionSuccessorParams{IdempotencyKey: "graduate-" + version},
				genclient.StableModuleVersionSuccessorBody{
					ExpectedPrereleaseResourceVersion: prerelease.ResourceVersion,
					Reason:                            "Graduate one concurrent stable successor",
					Version:                           lifecyclePublication(version),
				})
			results <- outcome{response: response, err: err}
		}()
	}
	close(start)
	var accepted *genclient.StableModuleVersionSuccessorResult
	rejected := 0
	for range stableVersions {
		result := <-results
		require.NoError(t, result.err)
		switch result.response.StatusCode() {
		case http.StatusCreated:
			require.Nil(t, accepted, "only one stable successor command may commit")
			accepted = result.response.JSON201
		case http.StatusConflict:
			rejected++
		default:
			t.Fatalf("unexpected stable successor status %d: %s", result.response.StatusCode(), result.response.Body)
		}
	}
	require.NotNil(t, accepted)
	require.Equal(t, len(stableVersions)-1, rejected)
	require.Equal(t, genclient.ModuleVersionSemanticStatus("deprecated"), accepted.Prerelease.LifecycleStatus)
	require.Equal(t, genclient.ModuleVersionSemanticStatus("proposed"), accepted.Stable.LifecycleStatus)
	require.NotEqual(t, prerelease.Uuid, accepted.Stable.Uuid)

	var totalVersions, proposedVersions, deprecatedPrereleases, commandReceipts, correlatedEvents int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT
		count(*),
		count(*) FILTER (WHERE semantic_status='proposed'),
		count(*) FILTER (WHERE uuid=$2 AND semantic_status='deprecated')
		FROM definition_versions WHERE org_id=$1`,
		orgID, prerelease.Uuid).Scan(&totalVersions, &proposedVersions, &deprecatedPrereleases))
	require.Equal(t, 3, totalVersions)
	require.Equal(t, 1, proposedVersions)
	require.Equal(t, 1, deprecatedPrereleases)
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM module_core_commands WHERE org_id=$1 AND command_scope=$2`,
		orgID, "stable-successor:"+moduleID+":"+prerelease.Uuid.String()).Scan(&commandReceipts))
	require.Equal(t, 1, commandReceipts)
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT count(*) FROM module_version_lifecycle_events WHERE org_id=$1 AND correlation_id=$2`,
		orgID, accepted.CorrelationId).Scan(&correlatedEvents))
	require.Equal(t, 2, correlatedEvents)
}
