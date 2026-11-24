//go:build integration

package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func nativeAgentTestConfiguration(t *testing.T) map[string]any {
	t.Helper()
	key, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKIXPublicKey(key)
	require.NoError(t, err)
	var catalogue map[string]any
	require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &catalogue))
	catalogue["runtime_version"] = "stellwerk.runner.rollout/v2"
	return map[string]any{
		"type": "native-agent", "key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})),
		"max_parallel_jobs": 2, "capabilities": []string{"local.exec"},
		"supported_protocols": []string{"stellwerk.pipeline/v2", "stellwerk.pipeline.plan-apply/v2"}, "native_execution": catalogue,
	}
}

func TestNativeAgentRunnerHTTPRegistrationLifecycle(t *testing.T) {
	db := pluginTestDatabase(t)
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "native-agent", CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-agent", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	call := func(method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		return response
	}
	base, path := "/orgs/native-agent/runners", "/orgs/native-agent/runners/pool"
	config := nativeAgentTestConfiguration(t)
	body := map[string]any{"id": "pool", "runner_configuration": config, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "retained"}}
	var created Runner
	require.NoError(t, json.Unmarshal(call(http.MethodPost, base, body, http.StatusCreated).Body.Bytes(), &created))
	require.NotNil(t, created.Uuid)
	require.NotEqual(t, uuid.Nil, *created.Uuid)
	stored, err := db.GetRunner(t.Context(), nil, "native-agent", "pool", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, "native-agent", stored.RunnerType)
	require.Nil(t, stored.RunnerConfigurationSecret, "native CRUD requires no Vault client or secret write")
	require.NotContains(t, stored.RunnerConfiguration, "job")
	for _, readPath := range []string{path, "/internal" + path} {
		var read Runner
		require.NoError(t, json.Unmarshal(call(http.MethodGet, readPath, nil, http.StatusOK).Body.Bytes(), &read))
		require.Equal(t, created.Uuid, read.Uuid)
	}
	listed := call(http.MethodGet, base, nil, http.StatusOK)
	require.NotContains(t, listed.Body.String(), "native_execution")
	var page RunnerPage
	require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &page))
	require.Equal(t, RunnerTypeNativeAgent, page.Items[0].RunnerConfiguration.Type)
	require.Equal(t, created.Uuid, page.Items[0].Uuid)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent"}}, http.StatusOK)
	preserved, err := db.GetRunner(t.Context(), nil, "native-agent", "pool", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, stored.RunnerConfiguration, preserved.RunnerConfiguration)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "native_execution": nil}}, http.StatusOK)
	call(http.MethodPatch, path, map[string]any{"description": "disabled pool"}, http.StatusOK)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "supported_protocols": []string{"stellwerk.pipeline/v1"}}}, http.StatusBadRequest)
	disabled, err := db.GetRunner(t.Context(), nil, "native-agent", "pool", model.GetModeDefault)
	require.NoError(t, err)
	require.NotContains(t, disabled.RunnerConfiguration, "native_execution")
	require.Equal(t, stored.Uuid, disabled.Uuid)
	require.Equal(t, stored.RunnerConfiguration["key"], disabled.RunnerConfiguration["key"])
	require.Equal(t, stored.RunnerConfiguration["max_parallel_jobs"], disabled.RunnerConfiguration["max_parallel_jobs"])
	call(http.MethodDelete, path, nil, http.StatusConflict)
	call(http.MethodPost, base, body, http.StatusConflict)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "supported_protocols": nil, "capabilities": nil}}, http.StatusOK)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": config}, http.StatusOK)
	replacement := nativeAgentTestConfiguration(t)["key"]
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "native-agent", "key": replacement, "max_parallel_jobs": 1}}, http.StatusOK)
	rotated, err := db.GetRunner(t.Context(), nil, "native-agent", "pool", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, stored.Uuid, rotated.Uuid)
	require.Equal(t, replacement, rotated.RunnerConfiguration["key"])
	require.InDelta(t, 1, rotated.RunnerConfiguration["max_parallel_jobs"], 0)
	require.Contains(t, rotated.RunnerConfiguration, "native_execution")
	call(http.MethodDelete, path, nil, http.StatusConflict)
	call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": "serverless-ecs"}}, http.StatusBadRequest)
	for backend, storage := range map[string]map[string]any{
		"s3":         {"type": "s3", "bucket": "retained"},
		"gcs":        {"type": "gcs", "bucket": "retained"},
		"azurerm":    {"type": "azurerm", "storage_account_name": "retained", "container_name": "state"},
		"kubernetes": {"type": "kubernetes", "namespace": "state"},
	} {
		call(http.MethodPost, base, map[string]any{"id": "backend-" + backend, "runner_configuration": config, "state_storage_configuration": storage}, http.StatusCreated)
	}
}

func TestNativeAgentRunnerHTTPRefusesInvalidConfiguration(t *testing.T) {
	db := pluginTestDatabase(t)
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: "native-invalid", CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-invalid", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Logger: zap.NewNop()}).MapRoutes(router)
	call := func(method, path string, config any, status int) {
		t.Helper()
		body := map[string]any{"runner_configuration": config}
		if method == http.MethodPost {
			body["id"], body["state_storage_configuration"] = "pool", map[string]any{"type": "s3", "bucket": "retained"}
		}
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		require.NotContains(t, response.Body.String(), "private-key-canary")
		require.NotContains(t, response.Body.String(), "private-canary")
	}
	base := "/orgs/native-invalid/runners"
	valid := nativeAgentTestConfiguration(t)
	for _, field := range []string{"key", "max_parallel_jobs", "supported_protocols", "capabilities", "native_execution"} {
		t.Run("required-"+field, func(t *testing.T) {
			config := nativeAgentTestConfiguration(t)
			config["native_execution"].(map[string]any)["tools"] = []any{}
			config["capabilities"] = []string{}
			delete(config, field)
			call(http.MethodPost, base, config, http.StatusBadRequest)
			config[field] = nil
			call(http.MethodPost, base, config, http.StatusBadRequest)
		})
	}
	for name, mutate := range map[string]func(map[string]any){
		"zero capacity": func(config map[string]any) { config["max_parallel_jobs"] = 0 },
		"invalid key":   func(config map[string]any) { config["key"] = "private-key-canary" },
		"legacy runtime": func(config map[string]any) {
			config["native_execution"].(map[string]any)["runtime_version"] = "stellwerk.runner.rollout/v1"
		},
		"missing saved plan": func(config map[string]any) { config["supported_protocols"] = []string{"stellwerk.pipeline/v2"} },
		"missing pipeline": func(config map[string]any) {
			config["supported_protocols"] = []string{"stellwerk.pipeline.plan-apply/v2"}
		},
		"legacy pipeline": func(config map[string]any) {
			config["supported_protocols"] = []string{"stellwerk.pipeline/v2", "stellwerk.pipeline.plan-apply/v2", "stellwerk.pipeline/v1"}
		},
		"legacy saved plan": func(config map[string]any) {
			config["supported_protocols"] = []string{"stellwerk.pipeline/v2", "stellwerk.pipeline.plan-apply/v2", "stellwerk.pipeline.plan-apply/v1"}
		},
		"job injection":     func(config map[string]any) { config["job"] = map[string]any{"namespace": "fake"} },
		"cluster injection": func(config map[string]any) { config["cluster"] = map[string]any{"server": "https://invalid"} },
		"UUID injection":    func(config map[string]any) { config["uuid"] = uuid.New() },
		"private inventory": func(config map[string]any) {
			config["native_execution"].(map[string]any)["environment"] = map[string]any{"KEY": "private-canary"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := nativeAgentTestConfiguration(t)
			mutate(config)
			call(http.MethodPost, base, config, http.StatusBadRequest)
		})
	}
	runners, _, err := db.ListRunners(t.Context(), nil, "native-invalid", "", 1)
	require.NoError(t, err)
	require.Empty(t, runners, "refused native Creates must leave no registration")
	call(http.MethodPost, base, valid, http.StatusCreated)
	before, err := db.GetRunner(t.Context(), nil, "native-invalid", "pool", model.GetModeDefault)
	require.NoError(t, err)
	for _, patch := range []map[string]any{
		{"key": nil}, {"key": "invalid"}, {"max_parallel_jobs": nil}, {"max_parallel_jobs": 0},
		{"supported_protocols": nil}, {"capabilities": nil}, {"job": map[string]any{}},
		{"supported_protocols": []string{"stellwerk.pipeline/v1"}},
	} {
		patch["type"] = "native-agent"
		call(http.MethodPatch, base+"/pool", patch, http.StatusBadRequest)
	}
	after, err := db.GetRunner(t.Context(), nil, "native-invalid", "pool", model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, before, after, "invalid patches change no retained configuration or identity")
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = db.CreateRunner(t.Context(), tx, &model.Runner{OrgId: "native-invalid", Id: "legacy", RunnerType: "serverless-ecs", CreatedAt: time.Now(), UpdatedAt: time.Now(), RunnerConfiguration: map[string]any{"type": "serverless-ecs"}, StateStorageType: "s3", StateStorageConfiguration: map[string]any{"type": "s3", "bucket": "retained"}})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	call(http.MethodPatch, base+"/legacy", valid, http.StatusBadRequest)
}
