package pluginmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/pluginstatus"
)

func TestCoreReadsOnlyAValidStatusReport(t *testing.T) {
	installation, digest := uuid.New(), strings.Repeat("c", 64)
	running := httptest.NewTLSServer(pluginstatus.Handler(func(context.Context) (pluginstatus.Report, error) {
		return pluginstatus.Report{InstallationID: installation.String(), PackageSHA256: digest, MigrationGeneration: 5}, nil
	}))
	defer running.Close()
	manager := &Manager{statusClient: running.Client()}
	observation := manager.readStatus(t.Context(), running.URL)
	require.Empty(t, observation.Failure)
	require.Equal(t, installation, *observation.InstallationID)
	require.Equal(t, digest, observation.PackageSHA256)
	require.EqualValues(t, 5, *observation.MigrationGeneration)
	require.Nil(t, diagnose(&observation, installation, digest, 5), "the plugin proves exactly the recorded package")

	stopped := httptest.NewServer(http.NotFoundHandler())
	stopped.Close()
	redirect := httptest.NewServer(http.RedirectHandler(running.URL+pluginstatus.Path, http.StatusTemporaryRedirect))
	defer redirect.Close()
	starting := httptest.NewServer(pluginstatus.Handler(func(context.Context) (pluginstatus.Report, error) {
		return pluginstatus.Report{}, context.DeadlineExceeded
	}))
	defer starting.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ready":true}`)) }))
	defer garbage.Close()
	for name, test := range map[string]struct{ backend, failure string }{
		"no plugin listening":      {stopped.URL, DiagnosticStatusUnreachable},
		"redirect is not followed": {redirect.URL, DiagnosticStatusInvalid},
		"plugin not ready":         {starting.URL, DiagnosticStatusInvalid},
		"not a status report":      {garbage.URL, DiagnosticStatusInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			observation := (&Manager{statusClient: http.DefaultClient}).readStatus(t.Context(), test.backend)
			require.Equal(t, test.failure, observation.Failure)
			require.NotEmpty(t, observation.Detail)
			require.LessOrEqual(t, len(observation.Detail), maxDetail)
			require.Nil(t, observation.InstallationID)
			diagnostic := diagnose(&observation, installation, digest, 5)
			require.NotNil(t, diagnostic)
			require.Equal(t, test.failure, diagnostic.Code)
		})
	}
}

func TestDiagnosisNamesTheFirstFactThatDiffers(t *testing.T) {
	installation, other, digest := uuid.New(), uuid.New(), strings.Repeat("d", 64)
	generation := int64(3)
	report := func(id uuid.UUID, sha string, generation int64) *model.PluginStatusObservation {
		return &model.PluginStatusObservation{InstallationID: &id, PackageSHA256: sha, MigrationGeneration: &generation}
	}
	for want, observation := range map[string]*model.PluginStatusObservation{
		DiagnosticInstallationMismatch: report(other, strings.Repeat("e", 64), 9),
		DiagnosticPackageMismatch:      report(installation, strings.Repeat("e", 64), 9),
		DiagnosticMigrationMismatch:    report(installation, digest, generation-1),
	} {
		diagnostic := diagnose(observation, installation, digest, generation)
		require.NotNil(t, diagnostic, want)
		require.Equal(t, want, diagnostic.Code)
		require.NotEmpty(t, diagnostic.Message)
	}
	require.Nil(t, diagnose(nil, installation, digest, generation), "nothing observed, nothing to diagnose")
}
