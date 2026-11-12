//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pluginmanager"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func maintenancePeer(t *testing.T, repo, pattern string, environment []string) string {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	stop := filepath.Join(dir, "stop")
	log, err := os.Create(filepath.Join(dir, "peer.log"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	cmd := exec.CommandContext(ctx, "go", "test", "-tags=integration", "-count=1", "-run", pattern, "./internal/api")
	if pattern == "^TestMaintenanceRuntimePeer$" {
		cmd.Args[len(cmd.Args)-1] = "./internal/service"
	}
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), append(environment, "GOTOOLCHAIN=go1.26.11", "MAINTENANCE_PEER_READY="+ready, "MAINTENANCE_PEER_STOP="+stop)...)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = os.WriteFile(stop, nil, 0600)
		select {
		case err := <-done:
			if err != nil {
				raw, _ := os.ReadFile(log.Name())
				t.Errorf("peer failed: %v\n%s", err, raw)
			}
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Error("peer did not stop")
		}
		cancel()
		_ = log.Close()
	})
	for until := time.Now().Add(40 * time.Second); time.Now().Before(until); {
		if raw, err := os.ReadFile(ready); err == nil {
			return string(raw)
		}
		select {
		case err := <-done:
			done <- err
			raw, _ := os.ReadFile(log.Name())
			t.Fatalf("peer startup failed: %v\n%s", err, raw)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("peer startup exceeded deadline")
	return ""
}

func TestInstallationMaintenanceWithRealDataPlaneAndPlugins(t *testing.T) {
	dpSource := os.Getenv("MAINTENANCE_DP_SOURCE")
	rolloutsSource := os.Getenv("MAINTENANCE_ROLLOUTS_SOURCE")
	driftSource := os.Getenv("MAINTENANCE_DRIFT_SOURCE")
	if dpSource == "" || rolloutsSource == "" || driftSource == "" {
		t.Skip("optional cross-repository qualification requires all 3 source paths")
	}
	dataPlane := maintenancePeer(t, dpSource, "^TestMaintenanceDataPlanePeer$", nil)
	db := pluginTestDatabase(t)
	server := Server{Database: db, MaintenanceDataPlaneURL: dataPlane}
	app := echo.New()
	// This fixture is the trusted service-network hop, not an IAM qualification.
	// Real authenticated plugin transports send a fixture credential identifying
	// only the principal assigned below; the Core handler still verifies its row.
	app.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Request().Header.Set("From", strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer "))
			return next(c)
		}
	})
	app.GET("/plugins/:pluginId/maintenance", server.getPluginMaintenance)
	core := httptest.NewTLSServer(app)
	defer core.Close()
	files := t.TempDir()
	ca := filepath.Join(files, "core.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: core.Certificate().Raw}), 0600))
	for index, item := range []struct{ id, source string }{{"dev.stellwerk.progressive-rollouts", rolloutsSource}, {"dev.stellwerk.drift-detection", driftSource}} {
		identity, service := uuid.New(), userid.NewServiceUserTokenId()
		digest := strings.Repeat([]string{"a", "b"}[index], 64)
		token := filepath.Join(files, item.id+".token")
		require.NoError(t, os.WriteFile(token, []byte(service.String()), 0600))
		backend := maintenancePeer(t, item.source, "^TestMaintenanceRuntimePeer$", []string{
			"MAINTENANCE_CORE_URL=" + core.URL, "MAINTENANCE_CORE_CA=" + ca, "MAINTENANCE_CORE_TOKEN_FILE=" + token,
			"STELLWERK_INSTALLATION_ID=" + identity.String(), "STELLWERK_PACKAGE_SHA256=" + digest,
		})
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO plugin_packages(manifest_sha256,plugin_id,version,manifest,created_by) VALUES($1,$2,'1.0.0','{}',$3)`, digest, item.id, uuid.New())
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO plugin_installations(installation_uuid,plugin_id,package_sha256,state,backend_url,service_user_id,admission_checker_id,created_by,updated_by) VALUES($1,$2,$3,'active',$4,$5,$6,$7,$7)`, identity, item.id, digest, backend, service, userid.NewServiceUserTokenId(), uuid.New())
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	value, err := server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ready", value.State, "%+v", value)
	require.Len(t, value.Plugins, 2)
	for _, plugin := range value.Plugins {
		require.True(t, plugin.Enabled)
		require.Equal(t, value.Revision, plugin.Revision)
	}
	body := bytes.NewBufferString(`{"enabled":false,"revision":2}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, dataPlane+"/test/maintenance", body)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	value, err = server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "running", value.State)
	for _, plugin := range value.Plugins {
		require.False(t, plugin.Enabled)
		require.EqualValues(t, 3, plugin.Revision)
	}
	// Re-enter maintenance to verify incompatible identity never produces ready.
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPut, dataPlane+"/test/maintenance", bytes.NewBufferString(`{"enabled":true,"revision":3}`))
	require.NoError(t, err)
	response, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	value, err = server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ready", value.State)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	replacement := strings.Repeat("c", 64)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO plugin_packages(manifest_sha256,plugin_id,version,manifest,created_by) VALUES($1,'dev.stellwerk.drift-detection','1.0.1','{}',$2)`, replacement, uuid.New())
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE plugin_installations SET package_sha256=$1 WHERE plugin_id='dev.stellwerk.drift-detection'`, replacement)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	value, err = server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "draining", value.State)
	require.NotEmpty(t, value.Plugins[0].Diagnostic)
	// An unsupported backend cannot be mistaken for an idle one.
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE plugin_installations SET backend_url=backend_url||'/unsupported' WHERE plugin_id='dev.stellwerk.drift-detection'`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	value, err = server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "draining", value.State)
	require.NotEmpty(t, value.Plugins[0].Diagnostic)
}

func TestMaintenanceReportsRequireEveryCountField(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": 2, "enabled": true})
	}))
	defer peer.Close()
	var report pluginMaintenanceReport
	require.Error(t, readMaintenanceJSON(t.Context(), peer.Client(), peer.URL, false, &report, "revision", "enabled", "unresolved", "parked"))
}

func TestMaintenanceReadinessRefusesInventoryChangeDuringReports(t *testing.T) {
	db := pluginTestDatabase(t)
	policy := model.InstallationMaintenance{Revision: 2, Enabled: true}
	require.NoError(t, model.ApplyInstallationMaintenance(t.Context(), db, policy))
	dataPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(maintenanceDP{InstallationMaintenance: policy})
	}))
	defer dataPlane.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	firstReport := sync.OnceFunc(func() { close(entered) })
	id := uuid.New()
	currentManifest := json.RawMessage(`{"id":"dev.example.paused","display_name":"Paused","version":"1.0.0"}`)
	digest := sha256Hex(currentManifest)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstReport()
		<-release
		_ = json.NewEncoder(w).Encode(pluginMaintenanceReport{InstallationID: id.String(), PackageSHA256: digest, InstallationMaintenance: policy})
	}))
	t.Cleanup(func() {
		unblock()
		backend.Close()
	})
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO plugin_packages(manifest_sha256,plugin_id,version,manifest,created_by) VALUES($1,'dev.example.paused','1.0.0',$2,$3)`, digest, currentManifest, uuid.New())
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO plugin_installations(installation_uuid,plugin_id,package_sha256,state,backend_url,created_by,updated_by) VALUES($1,'dev.example.paused',$2,'active',$3,$4,$4)`, id, digest, backend.URL, uuid.New())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	server := pluginManagerServer(t, db, operatorIAM(t))
	server.MaintenanceDataPlaneURL = dataPlane.URL
	server.MaintenanceHTTPClient = backend.Client()
	result := make(chan error, 1)
	go func() {
		_, err := server.installationMaintenanceStatus(t.Context())
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("plugin report was not requested")
	}
	// This must commit while the report is blocked: inventory locks cannot
	// cross network calls. Its revision then invalidates the collected proof.
	manifest := json.RawMessage(`{"id":"dev.example.next","display_name":"Next","version":"1.0.0"}`)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = server.PluginManager.PushPackage(ctx, pluginmanager.Command{Actor: pluginOperator,
		IdempotencyKey: uuid.NewString(), ExpectedRevision: 0, Reason: "Register the next package"}, sha256Hex(manifest), manifest)
	require.NoError(t, err)
	unblock()
	select {
	case err := <-result:
		require.ErrorContains(t, err, "inventory changed during inspection")
	case <-time.After(3 * time.Second):
		t.Fatal("changed inventory inspection did not finish")
	}
	value, err := server.installationMaintenanceStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ready", value.State, "the new inventory revision is stable on the next inspection")
}
