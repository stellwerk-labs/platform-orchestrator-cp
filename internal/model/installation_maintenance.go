package model

import (
	"context"
	"errors"
)

var ErrInstallationMaintenance = errors.New("installation maintenance pauses new plugin effects")

type InstallationMaintenance struct {
	Revision int64 `json:"revision"`
	Enabled  bool  `json:"enabled"`
}

func ReadInstallationMaintenance(ctx context.Context, db Databaser) (InstallationMaintenance, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return InstallationMaintenance{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var value InstallationMaintenance
	err = tx.QueryRowContext(ctx, `SELECT revision,enabled FROM installation_maintenance WHERE singleton`).Scan(&value.Revision, &value.Enabled)
	return value, err
}

// Apply takes the exclusive policy lock. Plugin effect transactions hold its
// shared counterpart until commit, so acknowledgement fences delayed senders.
func ApplyInstallationMaintenance(ctx context.Context, db Databaser, desired InstallationMaintenance) error {
	if desired.Revision < 1 {
		return errors.New("invalid maintenance revision")
	}
	current, err := ReadInstallationMaintenance(ctx, db)
	if err != nil {
		return err
	}
	if current == desired {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current = InstallationMaintenance{}
	if err = tx.QueryRowContext(ctx, `SELECT revision,enabled FROM installation_maintenance WHERE singleton FOR UPDATE`).Scan(&current.Revision, &current.Enabled); err != nil {
		return err
	}
	if desired.Revision < current.Revision || desired.Revision == current.Revision && desired.Enabled != current.Enabled {
		return errors.New("maintenance revision changed")
	}
	if desired.Revision > current.Revision {
		if _, err = tx.ExecContext(ctx, `UPDATE installation_maintenance SET revision=$1,enabled=$2 WHERE singleton`, desired.Revision, desired.Enabled); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func RequirePluginMaintenanceAdmission(ctx context.Context, tx Tx, action string) error {
	var value InstallationMaintenance
	if err := tx.QueryRowContext(ctx, `SELECT revision,enabled FROM installation_maintenance WHERE singleton FOR SHARE`).Scan(&value.Revision, &value.Enabled); err != nil {
		return err
	}
	if value.Revision < 1 {
		return ErrInstallationMaintenance
	}
	if !value.Enabled {
		return nil
	}
	// These actions only observe or settle existing work. Rollback admission,
	// Module restore and Pin begin are deliberately absent.
	switch action {
	case "external-deployment.read", "external-deployment.withdraw", "observation.grant.get", "observation.grant.revoke", "observation.get", "observation.request.get", "observation.redeploy.get", "observation.request.resolve", "observation.run.read", "observation.redeploy.resolve", "operation.suspend", "operation.release", "module.release", "pin.override.reconcile", "pin.override.restore", "pin.override.cancel", "events.publish":
		return nil
	default:
		return ErrInstallationMaintenance
	}
}
