//go:build integration

package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	iam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestMaintenanceBrowserPeer(t *testing.T) {
	ready := os.Getenv("PLATFORM_SETTINGS_CP_READY")
	if ready == "" {
		t.Skip("optional real IAM/Console qualification peer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	connection := os.Getenv("MODEL_TEST_DB_CONNECTION_STRING")
	admin, err := sql.Open("postgres", connection)
	require.NoError(t, err)
	defer admin.Close() //nolint:errcheck
	schema := "maintenance_browser_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck
	db, err := model.NewDatabaser(ctx, zap.NewNop(), connection+" options='-c search_path="+schema+"'")
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck
	certificate, err := os.ReadFile(os.Getenv("PLATFORM_SETTINGS_CA_FILE"))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certificate))
	client, err := iam.NewClientWithResponses(os.Getenv("PLATFORM_SETTINGS_IAM_URL"), iam.WithHTTPClient(&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}))
	require.NoError(t, err)
	server := Server{Database: db, IamClient: client, MaintenanceDataPlaneURL: os.Getenv("PLATFORM_SETTINGS_DP_URL")}
	app := echo.New()
	app.Use(middleware.EchoCtxMiddleware)
	server.MapRoutes(app)
	peer := httptest.NewServer(app)
	defer peer.Close()
	require.NoError(t, os.WriteFile(ready, []byte(peer.URL), 0600))
	for {
		if _, err := os.Stat(ready + ".stop"); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("maintenance browser peer exceeded lifetime")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
