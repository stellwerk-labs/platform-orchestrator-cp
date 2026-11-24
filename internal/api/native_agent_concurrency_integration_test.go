//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNativeAgentDisableSerializesConcurrentHTTPPatches(t *testing.T) {
	configuration := nativeAgentTestConfiguration(t)
	replacement := nativeAgentTestConfiguration(t)["key"]
	for _, tc := range []struct {
		name        string
		patch       map[string]any
		key         any
		description string
	}{
		{name: "description", patch: map[string]any{"description": "after disable"}, key: configuration["key"], description: "after disable"},
		{name: "key", patch: map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "key": replacement}}, key: replacement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := pluginTestDatabase(t)
			_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "native-race", CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
			require.NoError(t, err)
			router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-race", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
			require.NoError(t, err)
			router.Use(middleware.EchoCtxMiddleware)
			(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
			path := "/orgs/native-race/runners/pool"
			request := func(method, path string, body []byte) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, bytes.NewReader(body))
				req.Header.Set("From", userid.InternalSystemUuid.String())
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				return response
			}
			encoded, err := json.Marshal(map[string]any{"id": "pool", "runner_configuration": configuration, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "retained"}})
			require.NoError(t, err)
			created := request(http.MethodPost, "/orgs/native-race/runners", encoded)
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			beforeResponse := request(http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, beforeResponse.Code)
			var before Runner
			require.NoError(t, json.Unmarshal(beforeResponse.Body.Bytes(), &before))
			var beforeConfig map[string]any
			require.NoError(t, json.Unmarshal(before.RunnerConfiguration.union, &beforeConfig))
			held, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = held.Rollback() })
			var heldPID int
			require.NoError(t, held.QueryRowContext(t.Context(), `SELECT pg_backend_pid() FROM runners WHERE org_id='native-race' AND id='pool' FOR UPDATE`).Scan(&heldPID))
			waitBlocked := func(exclude int) int {
				t.Helper()
				var pid int
				var query string
				require.Eventually(t, func() bool {
					tx, err := db.BeginTx(t.Context(), nil)
					if err != nil {
						return false
					}
					defer func() { _ = tx.Rollback() }()
					err = tx.QueryRowContext(t.Context(), `SELECT pid,query FROM pg_stat_activity
					 WHERE pid<>$2 AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))) LIMIT 1`, heldPID, exclude).Scan(&pid, &query)
					return err == nil
				}, 10*time.Second, 5*time.Millisecond, "HTTP PATCH must actually be queued behind the owned Runner row")
				t.Logf("blocked HTTP PATCH: %s", query)
				return pid
			}
			cleared := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				cleared <- request(http.MethodPatch, path, []byte(`{"runner_configuration":{"type":"native-agent","native_execution":null}}`))
			}()
			clearPID := waitBlocked(0)
			encoded, err = json.Marshal(tc.patch)
			require.NoError(t, err)
			later := make(chan *httptest.ResponseRecorder, 1)
			go func() { later <- request(http.MethodPatch, path, encoded) }()
			waitBlocked(clearPID)
			require.NoError(t, held.Commit())
			for _, responses := range []chan *httptest.ResponseRecorder{cleared, later} {
				select {
				case response := <-responses:
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				case <-time.After(10 * time.Second):
					t.Fatal("queued HTTP PATCH did not complete after row release")
				}
			}
			afterResponse := request(http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, afterResponse.Code)
			var after Runner
			require.NoError(t, json.Unmarshal(afterResponse.Body.Bytes(), &after))
			var afterConfig map[string]any
			require.NoError(t, json.Unmarshal(after.RunnerConfiguration.union, &afterConfig))
			require.NotContains(t, afterConfig, "native_execution", "a stale HTTP PATCH must not re-enable a disabled native registration")
			require.Equal(t, before.Uuid, after.Uuid)
			require.Equal(t, beforeConfig["max_parallel_jobs"], afterConfig["max_parallel_jobs"])
			require.Equal(t, tc.key, afterConfig["key"])
			require.Equal(t, tc.description, ref.DerefOr(after.Description, ""))
		})
	}
}
