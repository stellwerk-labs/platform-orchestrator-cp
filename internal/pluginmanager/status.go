package pluginmanager

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/pluginstatus"
)

// Stable diagnostics of a running plugin that does not prove the package Core
// is to serve. An activation refused for one of them answers 409 with it.
const (
	DiagnosticStatusUnreachable    = "plugin_status_unreachable"
	DiagnosticStatusInvalid        = "plugin_status_invalid"
	DiagnosticInstallationMismatch = "plugin_installation_mismatch"
	DiagnosticPackageMismatch      = "plugin_package_digest_mismatch"
	DiagnosticMigrationMismatch    = "plugin_migration_generation_mismatch"
)

// statusTimeout bounds one status read; maxDetail bounds the recorded cause.
const (
	statusTimeout = 10 * time.Second
	maxDetail     = 500
)

// Diagnostic explains why the running plugin does not prove the package Core
// is to serve.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// readStatus reads the status the plugin behind backend reports. It never
// follows a redirect and reads at most one bounded report.
func (m *Manager) readStatus(ctx context.Context, backend string) model.PluginStatusObservation {
	observation := model.PluginStatusObservation{ObservedAt: time.Now().UTC()}
	failed := func(code string, err error) model.PluginStatusObservation {
		detail := strings.ToValidUTF8(err.Error(), "")
		if len(detail) > maxDetail {
			detail = strings.ToValidUTF8(detail[:maxDetail], "")
		}
		if detail == "" {
			detail = "no cause given"
		}
		observation.Failure, observation.Detail = code, detail
		return observation
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(backend, "/")+pluginstatus.Path, nil)
	if err != nil {
		return failed(DiagnosticStatusUnreachable, err)
	}
	client := *m.statusClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return failed(DiagnosticStatusUnreachable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return failed(DiagnosticStatusInvalid, fmt.Errorf("the status endpoint answered HTTP %d", response.StatusCode))
	}
	report, err := pluginstatus.Decode(response.Body)
	if err != nil {
		return failed(DiagnosticStatusInvalid, err)
	}
	reported, err := uuid.Parse(report.InstallationID)
	if err != nil {
		return failed(DiagnosticStatusInvalid, err)
	}
	observation.InstallationID, observation.PackageSHA256, observation.MigrationGeneration = &reported, report.PackageSHA256, &report.MigrationGeneration
	return observation
}

// diagnose compares what the plugin reported with the installation UUID,
// package digest and migration generation Core recorded. nil means the plugin
// proves exactly that package.
func diagnose(observation *model.PluginStatusObservation, installation uuid.UUID, digest string, generation int64) *Diagnostic {
	switch {
	case observation == nil:
		return nil
	case observation.Failure == DiagnosticStatusUnreachable:
		return &Diagnostic{observation.Failure, "Core could not read the plugin status: " + observation.Detail}
	case observation.Failure != "":
		return &Diagnostic{observation.Failure, "The plugin status is invalid: " + observation.Detail}
	case *observation.InstallationID != installation:
		return &Diagnostic{DiagnosticInstallationMismatch, fmt.Sprintf("The plugin serves installation %s, not %s. Configure the plugin runtime with the installation UUID Core assigned.", observation.InstallationID, installation)}
	case observation.PackageSHA256 != digest:
		return &Diagnostic{DiagnosticPackageMismatch, fmt.Sprintf("The plugin runs package %s, not %s. Activate again once the plugin runtime runs the recorded package.", observation.PackageSHA256, digest)}
	case *observation.MigrationGeneration != generation:
		return &Diagnostic{DiagnosticMigrationMismatch, fmt.Sprintf("The plugin database is at migration generation %d, not %d. Activate again once the package's migrations ran.", *observation.MigrationGeneration, generation)}
	}
	return nil
}
