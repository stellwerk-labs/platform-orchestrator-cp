package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Plugin Manager states. An installation serves registrations while active or
// disabled; disabled and organization-disabled plugins are safety-only.
const (
	PluginInstallationInstalled = "installed"
	PluginInstallationActive    = "active"
	PluginInstallationDisabled  = "disabled"
	PluginEnablementEnabled     = "enabled"
	PluginEnablementDisabled    = "disabled"
)

// Registration kinds materialised from a package on activation.
const (
	PluginRegistrationCapability          = "capability"
	PluginRegistrationOperation           = "operation"
	PluginRegistrationServiceGrant        = "service_grant"
	PluginRegistrationAuthorizationPolicy = "authorization_policy"
	PluginRegistrationUI                  = "ui"
)

// PluginPackage is an immutable registration manifest for one plugin version.
type PluginPackage struct {
	ManifestSHA256 string          `json:"manifest_sha256"`
	PluginID       string          `json:"plugin_id"`
	Version        string          `json:"version"`
	Manifest       json.RawMessage `json:"manifest"`
	CreatedAt      time.Time       `json:"created_at"`
	CreatedBy      uuid.UUID       `json:"created_by"`
}

// PluginInstallation is the operator record of one installed plugin.
type PluginInstallation struct {
	InstallationID     uuid.UUID          `json:"installation_id"`
	PluginID           string             `json:"plugin_id"`
	PackageSHA256      string             `json:"package_sha256"`
	State              string             `json:"state"`
	BackendURL         string             `json:"backend_url"`
	UIOrigin           string             `json:"ui_origin,omitempty"`
	ServiceUserID      string             `json:"service_user_id,omitempty"`
	AdmissionCheckerID string             `json:"admission_checker_id,omitempty"`
	Organizations      []PluginEnablement `json:"organizations"`
	CreatedAt          time.Time          `json:"created_at"`
	CreatedBy          uuid.UUID          `json:"created_by"`
	UpdatedAt          time.Time          `json:"updated_at"`
	UpdatedBy          uuid.UUID          `json:"updated_by"`
}

// PluginEnablement enables an installation for one organization.
type PluginEnablement struct {
	OrgID            string            `json:"org_id"`
	State            string            `json:"state"`
	Timezone         string            `json:"timezone"`
	ApprovalPolicies map[string]string `json:"approval_policies"`
	CreatedAt        time.Time         `json:"created_at"`
	CreatedBy        uuid.UUID         `json:"created_by"`
	UpdatedAt        time.Time         `json:"updated_at"`
	UpdatedBy        uuid.UUID         `json:"updated_by"`
}

// PluginRegistration is one contribution an installation serves.
type PluginRegistration struct {
	Kind       string
	Key        string
	Definition json.RawMessage
}

// PluginManagerAuditEvent is one committed Plugin Manager change.
type PluginManagerAuditEvent struct {
	ID               int64           `json:"id"`
	Revision         int64           `json:"revision"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Actor            uuid.UUID       `json:"actor"`
	Action           string          `json:"action"`
	InstallationID   *uuid.UUID      `json:"installation_id,omitempty"`
	OrgID            string          `json:"org_id,omitempty"`
	Reason           string          `json:"reason"`
	RequestID        string          `json:"request_id,omitempty"`
	IdempotencyKeyID string          `json:"idempotency_key_sha256"`
	Before           json.RawMessage `json:"before,omitempty"`
	After            json.RawMessage `json:"after,omitempty"`
}

// PluginManagerCommand identifies one operator write.
type PluginManagerCommand struct {
	Actor            uuid.UUID
	IdempotencyKey   string
	Fingerprint      string
	ExpectedRevision int64
	Reason           string
	RequestID        string
}

// PluginManagerChange describes what a command changed for the audit. A nil
// change means the requested state already holds.
type PluginManagerChange struct {
	Action         string
	InstallationID uuid.UUID
	OrgID          string
	Before, After  any
}

// ExecutePluginManagerCommand runs one operator write in one transaction:
//
//	lock plugin_manager_state           every operator write serialises here
//	stored response for the key?   ->   replay it; nothing changes
//	expected_revision == revision? ->   otherwise 409, nothing changes
//	mutate                              nil change: no revision, no audit
//	revision+1, one audit row, store the response, commit
//
// respond builds the response from the committed state and revision.
func ExecutePluginManagerCommand(ctx context.Context, db Databaser, command PluginManagerCommand, mutate func(context.Context, Tx) (*PluginManagerChange, error), respond func(context.Context, Tx, int64) (any, error)) (json.RawMessage, error) {
	reason := strings.TrimSpace(command.Reason)
	if command.Actor == uuid.Nil || command.IdempotencyKey == "" || command.Fingerprint == "" || reason == "" || len(reason) > 1000 || command.ExpectedRevision < 0 {
		return nil, ErrBadRequest{Code: "plugin_manager_request_invalid", Message: "An actor, an idempotency key, the expected revision and a reason of at most 1000 characters are required."}
	}
	key := sha256.Sum256([]byte(command.IdempotencyKey))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM plugin_manager_state FOR UPDATE`).Scan(&revision); err != nil {
		return nil, err
	}
	var fingerprint string
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT fingerprint, response FROM plugin_manager_commands WHERE idempotency_key_sha256=$1`, key[:]).Scan(&fingerprint, &stored)
	if err == nil {
		if fingerprint != command.Fingerprint {
			return nil, ErrConflict{Code: "plugin_manager_idempotency_conflict", Message: "The idempotency key was already used for a different Plugin Manager command."}
		}
		return stored, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if command.ExpectedRevision != revision {
		return nil, ErrConflict{Code: "plugin_manager_revision_conflict", Message: fmt.Sprintf("The Plugin Manager is at revision %d, not at the expected revision %d. Read the current state and retry.", revision, command.ExpectedRevision)}
	}
	change, err := mutate(ctx, tx)
	if err != nil {
		return nil, err
	}
	if change != nil {
		revision++
		if err := recordPluginManagerChange(ctx, tx, revision, command, reason, key, *change); err != nil {
			return nil, err
		}
	}
	response, err := respond(ctx, tx, revision)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_manager_commands (idempotency_key_sha256, fingerprint, actor, response) VALUES ($1,$2,$3,$4)`,
		key[:], command.Fingerprint, command.Actor, string(encoded)); err != nil {
		return nil, err
	}
	return encoded, tx.Commit()
}

func recordPluginManagerChange(ctx context.Context, tx Tx, revision int64, command PluginManagerCommand, reason string, key [32]byte, change PluginManagerChange) error {
	before, err := auditJSON(change.Before)
	if err != nil {
		return err
	}
	after, err := auditJSON(change.After)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_manager_state SET revision=$1`, revision); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugin_manager_audit (revision, actor, action, installation_uuid, org_id, reason, request_id, idempotency_key_sha256, before, after)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, revision, command.Actor, change.Action,
		uuid.NullUUID{UUID: change.InstallationID, Valid: change.InstallationID != uuid.Nil},
		sql.NullString{String: change.OrgID, Valid: change.OrgID != ""}, reason,
		sql.NullString{String: command.RequestID, Valid: command.RequestID != ""}, key[:], before, after)
	return err
}

func auditJSON(value any) (sql.NullString, error) {
	if value == nil {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(value)
	return sql.NullString{String: string(encoded), Valid: err == nil}, err
}

// PluginManagerRevision returns the revision the transaction observes.
func PluginManagerRevision(ctx context.Context, tx Tx) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM plugin_manager_state`).Scan(&revision)
	return revision, err
}

const pluginPackageColumns = `manifest_sha256, plugin_id, version, manifest, created_at, created_by`

func scanPluginPackage(row interface{ Scan(...any) error }) (PluginPackage, error) {
	var item PluginPackage
	var manifest []byte
	err := row.Scan(&item.ManifestSHA256, &item.PluginID, &item.Version, &manifest, &item.CreatedAt, &item.CreatedBy)
	item.Manifest = manifest
	return item, err
}

// GetPluginPackage returns a package by its manifest digest.
func GetPluginPackage(ctx context.Context, tx Tx, digest string) (PluginPackage, error) {
	item, err := scanPluginPackage(tx.QueryRowContext(ctx, `SELECT `+pluginPackageColumns+` FROM plugin_packages WHERE manifest_sha256=$1`, digest))
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound{Code: "plugin_package_not_found", Message: "The plugin package does not exist."}
	}
	return item, err
}

// ListPluginPackages returns every package ordered by plugin and version.
func ListPluginPackages(ctx context.Context, tx Tx) ([]PluginPackage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pluginPackageColumns+` FROM plugin_packages ORDER BY plugin_id, created_at, manifest_sha256`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []PluginPackage{}
	for rows.Next() {
		item, err := scanPluginPackage(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// InsertPluginPackage stores a new immutable package.
func InsertPluginPackage(ctx context.Context, tx Tx, item PluginPackage) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_packages (manifest_sha256, plugin_id, version, manifest, created_by) VALUES ($1,$2,$3,$4,$5)`,
		item.ManifestSHA256, item.PluginID, item.Version, string(item.Manifest), item.CreatedBy)
	if isUniqueViolation(err, "plugin_packages_plugin_id_version_key") {
		return ErrConflict{Code: "plugin_package_conflict", Message: "This plugin version is already registered with a different manifest digest."}
	}
	return err
}

const pluginInstallationColumns = `installation_uuid, plugin_id, package_sha256, state, backend_url, COALESCE(ui_origin,''),
	COALESCE(service_user_id::text,''), COALESCE(admission_checker_id::text,''), created_at, created_by, updated_at, updated_by`

func scanPluginInstallation(row interface{ Scan(...any) error }) (PluginInstallation, error) {
	var item PluginInstallation
	err := row.Scan(&item.InstallationID, &item.PluginID, &item.PackageSHA256, &item.State, &item.BackendURL, &item.UIOrigin,
		&item.ServiceUserID, &item.AdmissionCheckerID, &item.CreatedAt, &item.CreatedBy, &item.UpdatedAt, &item.UpdatedBy)
	return item, err
}

// GetPluginInstallation returns an installation with its enablements. lock
// takes the installation row for update.
func GetPluginInstallation(ctx context.Context, tx Tx, id uuid.UUID, lock bool) (PluginInstallation, error) {
	query := `SELECT ` + pluginInstallationColumns + ` FROM plugin_installations WHERE installation_uuid=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	item, err := scanPluginInstallation(tx.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound{Code: "plugin_installation_not_found", Message: "The plugin installation does not exist."}
	}
	if err != nil {
		return item, err
	}
	enablements, err := listPluginEnablements(ctx, tx, uuid.NullUUID{UUID: id, Valid: true})
	if err != nil {
		return item, err
	}
	item.Organizations = enablements[id]
	if item.Organizations == nil {
		item.Organizations = []PluginEnablement{}
	}
	return item, nil
}

// ListPluginInstallations returns every installation with its enablements.
func ListPluginInstallations(ctx context.Context, tx Tx) ([]PluginInstallation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pluginInstallationColumns+` FROM plugin_installations ORDER BY plugin_id, created_at, installation_uuid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []PluginInstallation{}
	for rows.Next() {
		item, err := scanPluginInstallation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	enablements, err := listPluginEnablements(ctx, tx, uuid.NullUUID{})
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Organizations = enablements[items[i].InstallationID]
		if items[i].Organizations == nil {
			items[i].Organizations = []PluginEnablement{}
		}
	}
	return items, nil
}

func listPluginEnablements(ctx context.Context, tx Tx, installation uuid.NullUUID) (map[uuid.UUID][]PluginEnablement, error) {
	rows, err := tx.QueryContext(ctx, `SELECT installation_uuid, org_id, state, timezone, approval_policies, created_at, created_by, updated_at, updated_by
		FROM plugin_enablements WHERE $1::uuid IS NULL OR installation_uuid=$1 ORDER BY org_id`, installation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[uuid.UUID][]PluginEnablement{}
	for rows.Next() {
		var id uuid.UUID
		var item PluginEnablement
		var policies []byte
		if err := rows.Scan(&id, &item.OrgID, &item.State, &item.Timezone, &policies, &item.CreatedAt, &item.CreatedBy, &item.UpdatedAt, &item.UpdatedBy); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(policies, &item.ApprovalPolicies); err != nil {
			return nil, err
		}
		result[id] = append(result[id], item)
	}
	return result, rows.Err()
}

// InsertPluginInstallation stores a new installation. Only one installation
// per plugin may exist until it is uninstalled.
func InsertPluginInstallation(ctx context.Context, tx Tx, item PluginInstallation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_installations (installation_uuid, plugin_id, package_sha256, state, backend_url, ui_origin, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`, item.InstallationID, item.PluginID, item.PackageSHA256, item.State, item.BackendURL,
		sql.NullString{String: item.UIOrigin, Valid: item.UIOrigin != ""}, item.CreatedBy)
	if isUniqueViolation(err, "plugin_installations_one_per_plugin") {
		return ErrConflict{Code: "plugin_installation_exists", Message: "The plugin already has an installation. Resume or change that installation instead."}
	}
	if isUniqueViolation(err, "plugin_installations_pkey") {
		return ErrConflict{Code: "plugin_installation_exists", Message: "The installation UUID is already assigned."}
	}
	return err
}

// UpdatePluginInstallation stores the state and identities of a locked
// installation.
func UpdatePluginInstallation(ctx context.Context, tx Tx, item PluginInstallation, actor uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `UPDATE plugin_installations SET state=$2, service_user_id=$3, admission_checker_id=$4, updated_at=now(), updated_by=$5
		WHERE installation_uuid=$1`, item.InstallationID, item.State,
		sql.NullString{String: item.ServiceUserID, Valid: item.ServiceUserID != ""},
		sql.NullString{String: item.AdmissionCheckerID, Valid: item.AdmissionCheckerID != ""}, actor)
	return err
}

// ReplacePluginRegistrations materialises the registrations of an
// installation's package and resynchronises the resource journal of its
// organizations, whose subscription depends on them.
func ReplacePluginRegistrations(ctx context.Context, tx Tx, installation PluginInstallation, registrations []PluginRegistration) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_extension_registrations WHERE installation_uuid=$1`, installation.InstallationID); err != nil {
		return err
	}
	for _, registration := range registrations {
		_, err := tx.ExecContext(ctx, `INSERT INTO plugin_extension_registrations (installation_uuid, package_sha256, kind, key, definition) VALUES ($1,$2,$3,$4,$5)`,
			installation.InstallationID, installation.PackageSHA256, registration.Kind, registration.Key, string(registration.Definition))
		if isUniqueViolation(err, "plugin_extension_registrations_ui_route") {
			return ErrConflict{Code: "plugin_registration_conflict", Message: "Another installation already serves the UI route prefix " + registration.Key + "."}
		}
		if err != nil {
			return err
		}
	}
	for _, enablement := range installation.Organizations {
		if err := syncExtensionResourceJournal(ctx, tx, enablement.OrgID); err != nil {
			return err
		}
	}
	return nil
}

// PutPluginEnablement creates or changes an organization's enablement and
// starts or pauses its resource journal in the same transaction.
func PutPluginEnablement(ctx context.Context, tx Tx, installation PluginInstallation, enablement PluginEnablement, actor uuid.UUID) error {
	policies, err := json.Marshal(enablement.ApprovalPolicies)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugin_enablements (plugin_id, org_id, installation_uuid, state, timezone, approval_policies, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		ON CONFLICT (plugin_id, org_id) DO UPDATE SET installation_uuid=EXCLUDED.installation_uuid, state=EXCLUDED.state, timezone=EXCLUDED.timezone,
			approval_policies=EXCLUDED.approval_policies, updated_at=now(), updated_by=EXCLUDED.updated_by`,
		installation.PluginID, enablement.OrgID, installation.InstallationID, enablement.State, enablement.Timezone, string(policies), actor)
	if isUniqueViolation(err, "plugin_enablements_installation_uuid_key") {
		return ErrConflict{Code: "plugin_enablement_limit", Message: "In v1alpha an installation is enabled for exactly one organization."}
	}
	if err != nil {
		return err
	}
	return syncExtensionResourceJournal(ctx, tx, enablement.OrgID)
}

// DeletePluginEnablement removes an organization's enablement and pauses its
// resource journal with a gap. It reports whether an enablement existed.
func DeletePluginEnablement(ctx context.Context, tx Tx, pluginID, orgID string) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM plugin_enablements WHERE plugin_id=$1 AND org_id=$2`, pluginID, orgID)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	if err != nil || deleted == 0 {
		return false, err
	}
	return true, syncExtensionResourceJournal(ctx, tx, orgID)
}

// ListPluginManagerAudit pages the audit trail in commit order.
func ListPluginManagerAudit(ctx context.Context, tx Tx, after int64, limit int) ([]PluginManagerAuditEvent, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, revision, occurred_at, actor, action, installation_uuid, COALESCE(org_id,''), reason, COALESCE(request_id,''),
		encode(idempotency_key_sha256,'hex'), before, after FROM plugin_manager_audit WHERE id>$1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []PluginManagerAuditEvent{}
	for rows.Next() {
		var item PluginManagerAuditEvent
		var installation uuid.NullUUID
		var before, after []byte
		if err := rows.Scan(&item.ID, &item.Revision, &item.OccurredAt, &item.Actor, &item.Action, &installation, &item.OrgID, &item.Reason, &item.RequestID,
			&item.IdempotencyKeyID, &before, &after); err != nil {
			return nil, err
		}
		if installation.Valid {
			item.InstallationID = &installation.UUID
		}
		item.Before, item.After = before, after
		items = append(items, item)
	}
	return items, rows.Err()
}

// PluginManagerSnapshot is everything a replica serves: the installations
// that have registrations, with package, registrations and enablements.
type PluginManagerSnapshot struct {
	Revision      int64
	Installations []PluginSnapshotInstallation
}

type PluginSnapshotInstallation struct {
	Installation  PluginInstallation
	Package       PluginPackage
	Registrations []PluginRegistration
}

// LoadPluginManagerSnapshot reads one consistent revision.
func LoadPluginManagerSnapshot(ctx context.Context, db Databaser) (PluginManagerSnapshot, error) {
	var snapshot PluginManagerSnapshot
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = tx.Rollback() }()
	if snapshot.Revision, err = PluginManagerRevision(ctx, tx); err != nil {
		return snapshot, err
	}
	installations, err := ListPluginInstallations(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	registrations, err := listPluginRegistrations(ctx, tx)
	if err != nil {
		return snapshot, err
	}
	for _, installation := range installations {
		if installation.State != PluginInstallationActive && installation.State != PluginInstallationDisabled {
			continue
		}
		pkg, err := GetPluginPackage(ctx, tx, installation.PackageSHA256)
		if err != nil {
			return snapshot, err
		}
		snapshot.Installations = append(snapshot.Installations, PluginSnapshotInstallation{Installation: installation, Package: pkg, Registrations: registrations[installation.InstallationID]})
	}
	return snapshot, tx.Commit()
}

func listPluginRegistrations(ctx context.Context, tx Tx) (map[uuid.UUID][]PluginRegistration, error) {
	rows, err := tx.QueryContext(ctx, `SELECT installation_uuid, kind, key, definition FROM plugin_extension_registrations ORDER BY installation_uuid, kind, key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[uuid.UUID][]PluginRegistration{}
	for rows.Next() {
		var id uuid.UUID
		var item PluginRegistration
		var definition []byte
		if err := rows.Scan(&id, &item.Kind, &item.Key, &definition); err != nil {
			return nil, err
		}
		item.Definition = definition
		result[id] = append(result[id], item)
	}
	return result, rows.Err()
}

// PluginUse names the installation a Core effect is performed for, as the
// caller's read snapshot saw it.
type PluginUse struct {
	OrgID          string
	PluginID       string
	InstallationID string
	PackageSHA256  string
	// Safety work (cancel, rollback, reconciliation) stays permitted while the
	// plugin is disabled for the organization or installation-wide.
	Safety bool
}

// RequirePluginEnablement re-verifies, inside the effect's own transaction,
// that the installation the caller's snapshot named is still the served one
// and is enabled for the organization. FOR SHARE holds both rows until the
// effect commits, so a concurrent disable either waits for the effect or the
// effect observes it. Every Core effect performed for a plugin calls this.
func RequirePluginEnablement(ctx context.Context, tx Tx, use PluginUse) error {
	var installationState, enablementState string
	err := tx.QueryRowContext(ctx, `SELECT i.state, e.state FROM plugin_enablements e
		JOIN plugin_installations i ON i.installation_uuid=e.installation_uuid
		WHERE e.plugin_id=$1 AND e.org_id=$2 AND i.installation_uuid::text=$3 AND i.package_sha256=$4
			AND i.state IN ('active','disabled')
		FOR SHARE OF e, i`, use.PluginID, use.OrgID, use.InstallationID, use.PackageSHA256).Scan(&installationState, &enablementState)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict{Code: "plugin_not_installed", Message: "The plugin installation is no longer installed and enabled for this organization."}
	}
	if err != nil {
		return err
	}
	if !use.Safety && (installationState != PluginInstallationActive || enablementState != PluginEnablementEnabled) {
		return ErrConflict{Code: "plugin_not_enabled", Message: "The plugin is not enabled for this organization."}
	}
	return nil
}

// pluginSubscribesToResourceEvents reports whether the plugin is enabled for
// the organization with a registered resource event read grant.
func pluginSubscribesToResourceEvents(ctx context.Context, tx Tx, org, plugin string) (bool, error) {
	var subscribed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM plugin_enablements e
		JOIN plugin_extension_registrations r ON r.installation_uuid=e.installation_uuid AND r.kind=$3 AND r.key=$4
		WHERE e.org_id=$1 AND e.plugin_id=$2)`, org, plugin, PluginRegistrationServiceGrant, extensionResourceEventsRead).Scan(&subscribed)
	return subscribed, err
}

// resourceEventSubscribers returns the plugins subscribed to the
// organization's resource event journal.
func resourceEventSubscribers(ctx context.Context, tx Tx, org string) ([]string, error) {
	var plugins pq.StringArray
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(array_agg(DISTINCT e.plugin_id ORDER BY e.plugin_id),'{}') FROM plugin_enablements e
		JOIN plugin_extension_registrations r ON r.installation_uuid=e.installation_uuid AND r.kind=$2 AND r.key=$3
		WHERE e.org_id=$1`, org, PluginRegistrationServiceGrant, extensionResourceEventsRead).Scan(&plugins)
	return plugins, err
}
