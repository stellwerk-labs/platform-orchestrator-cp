//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRunnerNativeExecutionHTTPRoundTrip(t *testing.T) {
	db := pluginTestDatabase(t)
	org := "native-" + uuid.NewString()[:8]
	_, err := db.CreateOrg(t.Context(), nil, &model.Org{Id: org, CreatedBy: userid.InternalSystemUuid, CreatedAt: time.Now(), Source: model.OrgSourceInternal, Plan: model.OrgPlanCustom})
	require.NoError(t, err)
	address, token := os.Getenv("RUNNER_TEST_VAULT_ADDR"), os.Getenv("RUNNER_TEST_VAULT_TOKEN")
	require.NotEmpty(t, address, "real Vault is required for private Runner configuration round trips")
	require.NotEmpty(t, token)
	vaultClient, err := vaultapi.NewClient(&vaultapi.Config{Address: address})
	require.NoError(t, err)
	vaultClient.SetToken(token)
	secretStore := vault.NewVaultClient(vaultClient, zap.NewNop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, secretStore.DeleteSecret(ctx, credsRunnerSecretPath(org, "kubernetes")))
	})
	router, err := hecho.DefaultEchoServerWithValidation(&hecho.ValidatedServerConfig{AppName: "native-catalogue-round-trip", Logger: zap.NewNop(), OpenAPIRawSchema: MustDecodeOpenApiSpec()})
	require.NoError(t, err)
	router.Use(middleware.EchoCtxMiddleware)
	(&Server{Database: db, Vault: secretStore, Logger: zap.NewNop()}).MapRoutes(router)
	call := func(method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(data))
		request.Header.Set("From", userid.InternalSystemUuid.String())
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		return response
	}
	key, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	public, err := x509.MarshalPKIXPublicKey(key)
	require.NoError(t, err)
	job := map[string]any{"namespace": "native", "service_account": "native"}
	auth := map[string]any{"role_arn": "arn:aws:iam::123456789012:role/NativeRunner"}
	for runnerType, config := range map[string]map[string]any{
		"kubernetes":       {"job": job, "cluster": map[string]any{"auth": map[string]any{"service_account_token": "private-runner-canary"}, "cluster_data": map[string]any{"server": "https://cluster.invalid", "certificate_authority_data": "fixture"}}},
		"kubernetes-agent": {"job": job, "key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))},
		"kubernetes-eks":   {"job": job, "cluster": map[string]any{"name": "native", "region": "us-east-1", "auth": auth}},
		"kubernetes-gke":   {"job": job, "cluster": map[string]any{"name": "native", "location": "us-central1", "project_id": "native", "auth": map[string]any{"gcp_audience": "native", "gcp_service_account": "native@example.invalid"}}},
		"serverless-ecs":   {"auth": auth, "job": map[string]any{"region": "us-east-1", "cluster": "native", "subnets": []string{"subnet-native"}, "execution_role_arn": "arn:aws:iam::123456789012:role/NativeExecution"}},
	} {
		t.Run(runnerType, func(t *testing.T) {
			var catalogue map[string]any
			require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &catalogue))
			config["type"], config["native_execution"] = runnerType, catalogue
			config["supported_protocols"], config["capabilities"] = []string{"stellwerk.pipeline/v1"}, []string{"local.exec"}
			base := "/orgs/" + org + "/runners"
			created := call(http.MethodPost, base, map[string]any{"id": runnerType, "runner_configuration": config, "state_storage_configuration": map[string]any{"type": "s3", "bucket": "native"}}, http.StatusCreated)
			require.NotContains(t, created.Body.String(), "private-runner-canary")
			stored, err := db.GetRunner(t.Context(), nil, org, runnerType, model.GetModeDefault)
			require.NoError(t, err)
			path := base + "/" + runnerType
			call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": runnerType}}, http.StatusOK)
			after, err := db.GetRunner(t.Context(), nil, org, runnerType, model.GetModeDefault)
			require.NoError(t, err)
			require.Equal(t, stored.RunnerConfiguration, after.RunnerConfiguration)
			require.Equal(t, stored.RunnerConfigurationSecret, after.RunnerConfigurationSecret)
			call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": runnerType, "capabilities": nil}}, http.StatusBadRequest)
			call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": runnerType, "supported_protocols": nil}}, http.StatusBadRequest)
			catalogue["tools_digest"] = strings.Repeat("b", 64)
			replaced := call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": runnerType, "native_execution": catalogue}}, http.StatusOK)
			require.Contains(t, replaced.Body.String(), strings.Repeat("b", 64))
			full := call(http.MethodGet, path, nil, http.StatusOK)
			require.Contains(t, full.Body.String(), `"native_execution"`)
			require.NotContains(t, full.Body.String(), "private-runner-canary")
			call(http.MethodPatch, path, map[string]any{"runner_configuration": map[string]any{"type": runnerType, "native_execution": nil, "capabilities": nil, "supported_protocols": nil}}, http.StatusOK)
			after, err = db.GetRunner(t.Context(), nil, org, runnerType, model.GetModeDefault)
			require.NoError(t, err)
			require.NotContains(t, after.RunnerConfiguration, "native_execution")
			require.Equal(t, stored.RunnerConfigurationSecret, after.RunnerConfigurationSecret)
		})
	}
	// An invalid inventory must be rejected before a supplied credential can
	// create a new Vault version, even though the database pointer would stay old.
	var validCatalogue map[string]any
	require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &validCatalogue))
	call(http.MethodPatch, "/orgs/"+org+"/runners/kubernetes", map[string]any{"runner_configuration": map[string]any{
		"type": "kubernetes", "native_execution": validCatalogue, "supported_protocols": []string{"stellwerk.pipeline/v1"},
		"cluster": map[string]any{"auth": map[string]any{"service_account_token": "must-not-write"}, "cluster_data": map[string]any{"server": "https://cluster.invalid", "certificate_authority_data": "fixture"}},
	}}, http.StatusBadRequest)
	secret, err := vaultClient.KVv2("secret").Get(t.Context(), credsRunnerSecretPath(org, "kubernetes"))
	require.NoError(t, err)
	require.Equal(t, 1, secret.VersionMetadata.Version)
	encoded, err := json.Marshal(secret.Data)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "private-runner-canary")
	var invalidCatalogue map[string]any
	require.NoError(t, json.Unmarshal([]byte(nativeCatalogueFixture), &invalidCatalogue))
	invalidCatalogue["tools"].([]any)[0].(map[string]any)["environment"] = map[string]any{"API_KEY": "private-inventory-canary"}
	invalid := call(http.MethodPatch, "/orgs/"+org+"/runners/serverless-ecs", map[string]any{"runner_configuration": map[string]any{"type": "serverless-ecs", "native_execution": invalidCatalogue}}, http.StatusBadRequest)
	require.NotContains(t, invalid.Body.String(), "private-inventory-canary")
}
