package pluginmanager

import (
	"context"

	"github.com/google/uuid"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

// PackagePage lists every package at one revision.
type PackagePage struct {
	Revision int64                 `json:"revision"`
	Items    []model.PluginPackage `json:"items"`
}

// InstallationPage lists every installation at one revision.
type InstallationPage struct {
	Revision int64                      `json:"revision"`
	Items    []model.PluginInstallation `json:"items"`
}

// AuditPage is one page of the audit trail.
type AuditPage struct {
	Items []model.PluginManagerAuditEvent `json:"items"`
}

// MaxAuditPage bounds one audit page.
const MaxAuditPage = 200

func (m *Manager) Packages(ctx context.Context) (PackagePage, error) {
	page := PackagePage{}
	err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		if page.Revision, err = model.PluginManagerRevision(ctx, tx); err != nil {
			return err
		}
		page.Items, err = model.ListPluginPackages(ctx, tx)
		return err
	})
	return page, err
}

func (m *Manager) Installations(ctx context.Context) (InstallationPage, error) {
	page := InstallationPage{}
	err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		if page.Revision, err = model.PluginManagerRevision(ctx, tx); err != nil {
			return err
		}
		page.Items, err = model.ListPluginInstallations(ctx, tx)
		return err
	})
	return page, err
}

func (m *Manager) Installation(ctx context.Context, id uuid.UUID) (Installation, error) {
	result := Installation{}
	err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		if result.Revision, err = model.PluginManagerRevision(ctx, tx); err != nil {
			return err
		}
		result.PluginInstallation, err = model.GetPluginInstallation(ctx, tx, id, false)
		return err
	})
	return result, err
}

// AuditEvents pages the audit trail after the given event ID.
func (m *Manager) AuditEvents(ctx context.Context, after int64, limit int) (AuditPage, error) {
	if after < 0 || limit < 1 || limit > MaxAuditPage {
		return AuditPage{}, model.ErrBadRequest{Code: "plugin_manager_request_invalid", Message: "The audit page needs a nonnegative cursor and a limit between 1 and 200."}
	}
	page := AuditPage{}
	err := readOnly(ctx, m.db, func(tx model.Tx) (err error) {
		page.Items, err = model.ListPluginManagerAudit(ctx, tx, after, limit)
		return err
	})
	return page, err
}
