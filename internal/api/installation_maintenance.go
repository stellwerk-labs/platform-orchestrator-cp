package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/golib/hecho"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	iam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
)

type maintenanceDP struct {
	model.InstallationMaintenance
	Executing    int64 `json:"executing_deployments"`
	Queued       int64 `json:"queued_deployments"`
	Runners      int64 `json:"runner_reservations"`
	Admissions   int64 `json:"unsettled_admissions"`
	Observations int64 `json:"executing_observations"`
}

type pluginMaintenanceReport struct {
	InstallationID string `json:"installation_id"`
	PackageSHA256  string `json:"package_sha256"`
	model.InstallationMaintenance
	Unresolved int64 `json:"unresolved"`
	Parked     int64 `json:"parked"`
}

type maintenancePlugin struct {
	PluginID string `json:"plugin_id"`
	pluginMaintenanceReport
	Diagnostic string `json:"diagnostic,omitempty"`
	backend    string
}

type maintenanceStatus struct {
	Revision     int64               `json:"revision"`
	Enabled      bool                `json:"enabled"`
	State        string              `json:"state"`
	DataPlane    maintenanceDP       `json:"data_plane"`
	Dispatches   int64               `json:"unsettled_dispatches"`
	Observations int64               `json:"unresolved_observations"`
	Plugins      []maintenancePlugin `json:"plugins"`
	Diagnostic   string              `json:"diagnostic,omitempty"`
}

func SkipMaintenanceOpenAPIValidation(c echo.Context) bool {
	return c.Path() == "/plugins/:pluginId/maintenance" || c.Path() == "/platform-settings/maintenance-status" || hecho.DefaultOAIValidationSkipper(c)
}

// readMaintenanceJSON never follows redirects or accepts an unbounded report.
func readMaintenanceJSON(ctx context.Context, client *http.Client, endpoint string, internal bool, value any, required ...string) error {
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if internal {
		request.Header.Set("From", userid.InternalSystemUuid.String())
	}
	response, err := bounded.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("maintenance status unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil || len(body) > 16384 {
		return errors.New("invalid maintenance report")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	for _, key := range required {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return errors.New("incomplete maintenance report")
		}
	}
	return json.Unmarshal(body, value)
}

func (s *Server) maintenanceDesired(ctx context.Context) (maintenanceDP, error) {
	var value maintenanceDP
	if s.MaintenanceDataPlaneURL == "" {
		return value, errors.New("maintenance source unavailable")
	}
	err := readMaintenanceJSON(ctx, s.MaintenanceHTTPClient, strings.TrimSuffix(s.MaintenanceDataPlaneURL, "/")+"/internal/installation-maintenance", true, &value, "revision", "enabled", "executing_deployments", "queued_deployments", "runner_reservations", "unsettled_admissions", "executing_observations")
	if err != nil {
		return value, err
	}
	if value.Revision < 1 || value.Executing < 0 || value.Queued < 0 || value.Runners < 0 || value.Admissions < 0 || value.Observations < 0 {
		return value, errors.New("invalid maintenance revision")
	}
	return value, model.ApplyInstallationMaintenance(ctx, s.Database, value.InstallationMaintenance)
}

func (s *Server) ReconcileInstallationMaintenance(ctx context.Context) error {
	_, err := s.maintenanceDesired(ctx)
	return err
}

func (s *Server) getPluginMaintenance(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	actor, err := uuid.Parse(c.Request().Header.Get("From"))
	if err != nil || !userid.IsServiceUser(actor) {
		return echo.NewHTTPError(403, "Plugin service identity required")
	}
	tx, err := s.Database.BeginTx(c.Request().Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id, digest string
	err = tx.QueryRowContext(c.Request().Context(), `SELECT installation_uuid::text,package_sha256 FROM plugin_installations WHERE plugin_id=$1 AND service_user_id=$2 AND state IN ('active','disabled')`, c.Param("pluginId"), actor).Scan(&id, &digest)
	if err != nil {
		return echo.NewHTTPError(403, "Installed plugin service identity required")
	}
	// Release the read transaction before contacting DP, whose admissions call CP.
	_ = tx.Rollback()
	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
	defer cancel()
	desired, err := s.maintenanceDesired(ctx)
	if err != nil {
		return echo.NewHTTPError(503, "Maintenance state unavailable")
	}
	return c.JSON(200, pluginMaintenanceReport{InstallationID: id, PackageSHA256: digest, InstallationMaintenance: desired.InstallationMaintenance})
}

func (s *Server) requireMaintenanceReader(c echo.Context) error {
	actor, err := uuid.Parse(c.Request().Header.Get("From"))
	if err != nil || !userid.IsHumanUser(actor) || s.IamClient == nil {
		return echo.NewHTTPError(403, "Platform settings permission required")
	}
	response, err := s.IamClient.InternalAuthorizeWithResponse(c.Request().Context(), iam.InternalAuthorizeBody{UserId: actor, Checks: []iam.ResourcePermissionCheck{{Resource: "platform", Permission: "platform_settings_read"}}})
	if err != nil {
		return echo.NewHTTPError(503, "Platform authorization unavailable")
	}
	if response.StatusCode() != http.StatusNoContent {
		return echo.NewHTTPError(403, "Platform settings permission required")
	}
	return nil
}

func (s *Server) getInstallationMaintenanceStatus(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	if err := s.requireMaintenanceReader(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
	defer cancel()
	value, err := s.installationMaintenanceStatus(ctx)
	if err != nil {
		return c.JSON(200, maintenanceStatus{State: "unknown", Plugins: []maintenancePlugin{}, Diagnostic: "Maintenance state could not be verified."})
	}
	return c.JSON(200, value)
}

func (s *Server) maintenanceInventory(ctx context.Context) ([]maintenancePlugin, int64, error) {
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM plugin_manager_state FOR SHARE`).Scan(&revision); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT plugin_id,installation_uuid::text,package_sha256,backend_url FROM plugin_installations WHERE state IN ('active','disabled') ORDER BY plugin_id LIMIT 201`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := []maintenancePlugin{}
	for rows.Next() {
		var item maintenancePlugin
		if err = rows.Scan(&item.PluginID, &item.InstallationID, &item.PackageSHA256, &item.backend); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if len(items) > 200 {
		return nil, 0, errors.New("maintenance inventory exceeds bounded read")
	}
	return items, revision, rows.Err()
}

func (s *Server) maintenancePluginReports(ctx context.Context, desired maintenanceDP) ([]maintenancePlugin, bool, int64, error) {
	items, revision, err := s.maintenanceInventory(ctx)
	if err != nil {
		return nil, false, 0, err
	}
	converged := true
	for i := range items {
		item := &items[i]
		var report pluginMaintenanceReport
		client := s.MaintenanceHTTPClient
		if s.PluginGateway != nil && s.PluginGateway.Client != nil {
			client = s.PluginGateway.Client
		}
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = readMaintenanceJSON(bounded, client, strings.TrimSuffix(item.backend, "/")+"/v1/maintenance", false, &report, "installation_id", "package_sha256", "revision", "enabled", "unresolved", "parked")
		cancel()
		if err != nil || report.InstallationID != item.InstallationID || report.PackageSHA256 != item.PackageSHA256 || report.Revision != desired.Revision || report.Enabled != desired.Enabled || report.Unresolved < 0 || report.Parked < 0 {
			item.Diagnostic = "Plugin pause state could not be verified."
			converged = false
			continue
		}
		item.pluginMaintenanceReport = report
		if report.Unresolved != 0 {
			converged = converged && !desired.Enabled
		}
	}
	return items, converged, revision, nil
}

func (s *Server) installationMaintenanceStatus(ctx context.Context) (maintenanceStatus, error) {
	desired, err := s.maintenanceDesired(ctx)
	out := maintenanceStatus{Revision: desired.Revision, Enabled: desired.Enabled, State: "running", DataPlane: desired}
	if err != nil {
		return out, err
	}
	var converged bool
	var inventoryRevision int64
	out.Plugins, converged, inventoryRevision, err = s.maintenancePluginReports(ctx, desired)
	if err != nil {
		return out, err
	}
	final, err := s.maintenanceDesired(ctx)
	if err != nil || final.InstallationMaintenance != desired.InstallationMaintenance {
		return out, errors.New("maintenance changed during inspection")
	}
	out.DataPlane = final
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var inventoryRevisionAfter int64
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM external_operation_dispatches WHERE state<>'settled'),(SELECT count(*) FROM environment_observation_requests r WHERE response IS NULL AND NOT EXISTS(SELECT 1 FROM environment_observation_closures c WHERE c.org_id=r.org_id AND c.plugin_id=r.plugin_id AND c.request_id=r.request_id AND c.kind='observation' AND c.state IN ('accepted','not_started')))+(SELECT count(*) FROM environment_observation_closures WHERE state='close_requested'),(SELECT revision FROM plugin_manager_state)`).Scan(&out.Dispatches, &out.Observations, &inventoryRevisionAfter)
	if err != nil {
		return out, err
	}
	_ = tx.Rollback()
	if inventoryRevisionAfter != inventoryRevision {
		return out, errors.New("plugin inventory changed during inspection")
	}
	if !desired.Enabled {
		if !converged {
			out.State = "resuming"
		}
		return out, nil
	}
	out.State = "draining"
	if converged && out.Dispatches == 0 && out.Observations == 0 && final.Executing == 0 && final.Queued == 0 && final.Runners == 0 && final.Admissions == 0 && final.Observations == 0 {
		out.State = "ready"
	}
	return out, nil
}
