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
// disabled; disabled and organization-disabled plugins are safety-only. An
// uninstalled installation keeps its UUID and data for a reinstall; a purged
// one is retired for good.
const (
	PluginInstallationInstalled   = "installed"
	PluginInstallationActive      = "active"
	PluginInstallationDisabled    = "disabled"
	PluginInstallationUninstalled = "uninstalled"
	PluginInstallationPurged      = "purged"
	PluginEnablementEnabled       = "enabled"
	PluginEnablementDisabled      = "disabled"
)

// Organization modes of an installation. With selected the operator enables
// each organization; with all Core enables every organization, existing and
// created later, unless the operator disabled it.
const (
	PluginOrganizationsSelected = "selected"
	PluginOrganizationsAll      = "all"
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
	InstallationID uuid.UUID `json:"installation_id"`
	PluginID       string    `json:"plugin_id"`
	PackageSHA256  string    `json:"package_sha256"`
	// TargetPackageSHA256 is the package of a pending upgrade. Core serves
	// PackageSHA256 until an activation reads the target from the plugin.
	TargetPackageSHA256 string `json:"target_package_sha256,omitempty"`
	State               string `json:"state"`
	BackendURL          string `json:"backend_url"`
	UIOrigin            string `json:"ui_origin,omitempty"`
	ServiceUserID       string `json:"service_user_id,omitempty"`
	AdmissionCheckerID  string `json:"admission_checker_id,omitempty"`
	// OrganizationMode is selected or all.
	OrganizationMode string `json:"organization_mode"`
	// OrganizationDefaults are the timezone and approval policies of every
	// enablement Core creates on its own in mode all.
	OrganizationDefaults PluginOrganizationDefaults `json:"organization_defaults"`
	Organizations        []PluginEnablement         `json:"organizations"`
	CreatedAt            time.Time                  `json:"created_at"`
	CreatedBy            uuid.UUID                  `json:"created_by"`
	UpdatedAt            time.Time                  `json:"updated_at"`
	UpdatedBy            uuid.UUID                  `json:"updated_by"`
	// Observation is what Core last read from the running plugin's status.
	Observation *PluginStatusObservation `json:"observation,omitempty"`
}

// PluginStatusObservation is what Core last read from the status endpoint of
// an installation's running plugin: the report, or why none could be read.
type PluginStatusObservation struct {
	ObservedAt          time.Time  `json:"observed_at"`
	Failure             string     `json:"failure,omitempty"`
	Detail              string     `json:"detail,omitempty"`
	InstallationID      *uuid.UUID `json:"installation_id,omitempty"`
	PackageSHA256       string     `json:"package_sha256,omitempty"`
	MigrationGeneration *int64     `json:"migration_generation,omitempty"`
}

// PluginOrganizationDefaults configure the enablements Core creates on its
// own. Changing them changes no existing enablement.
type PluginOrganizationDefaults struct {
	Timezone         string            `json:"timezone"`
	ApprovalPolicies map[string]string `json:"approval_policies"`
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
	// CapabilityRegistration tells whether IAM holds the capability
	// declarations of the served package for this organization.
	CapabilityRegistration PluginCapabilityRegistration `json:"capability_registration"`
	// EntitlementGrant is the grant that covers the organization when the
	// served package requires a licence: the organization's own grant or
	// else the installation-wide grant. Reads derive it; it is not stored.
	EntitlementGrant *PluginEntitlementGrantStatus `json:"entitlement_grant,omitempty"`
}

// Capability registration states of an enablement.
const (
	// PluginCapabilitiesRegistered: Core registered or confirmed the served
	// package's declarations in IAM.
	PluginCapabilitiesRegistered = "registered"
	// PluginCapabilitiesPending: Core has not reconciled the served package
	// for this organization yet.
	PluginCapabilitiesPending = "pending"
	// PluginCapabilitiesFailed: Core's last attempt for the served package
	// failed; it tries again.
	PluginCapabilitiesFailed = "failed"
)

// PluginCapabilityRegistration is the state of an organization's capability
// registration, derived from Core's last reconciliation.
type PluginCapabilityRegistration struct {
	State string `json:"state"`
	// PackageSHA256 is the package Core last reconciled for the
	// organization, which may precede the served one.
	PackageSHA256 string     `json:"package_sha256,omitempty"`
	ReconciledAt  *time.Time `json:"reconciled_at,omitempty"`
	Detail        string     `json:"detail,omitempty"`
}

// PluginCapabilityReconciliation is what Core did to bring IAM's capability
// catalogue of a plugin in one organization in line with a package.
type PluginCapabilityReconciliation struct {
	PluginID      string
	OrgID         string
	PackageSHA256 string
	ReconciledAt  time.Time
	// Failure says why IAM does not provably hold the declarations.
	Failure string
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
	revision, err := LockPluginManager(ctx, tx)
	if err != nil {
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

// LockPluginManager serialises a change Core makes on its own with every
// operator command until the transaction ends, and returns the revision.
func LockPluginManager(ctx context.Context, tx Tx) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM plugin_manager_state FOR UPDATE`).Scan(&revision)
	return revision, err
}

// RecordPluginManagerSystemChange records a change Core made on its own, in
// a transaction that holds LockPluginManager at revision: the next revision
// and one audit row. Where an operator command's audit shows the digest of
// its idempotency key, this one shows the digest of cause, which names the
// event that made Core act. It returns the new revision.
func RecordPluginManagerSystemChange(ctx context.Context, tx Tx, revision int64, actor uuid.UUID, cause, reason string, change PluginManagerChange) (int64, error) {
	revision++
	return revision, recordPluginManagerChange(ctx, tx, revision, PluginManagerCommand{Actor: actor}, reason, sha256.Sum256([]byte(cause)), change)
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

const pluginInstallationColumns = `i.installation_uuid, i.plugin_id, i.package_sha256, COALESCE(i.target_package_sha256,''), i.state, i.backend_url, COALESCE(i.ui_origin,''),
	COALESCE(i.service_user_id::text,''), COALESCE(i.admission_checker_id::text,''), i.organization_mode, i.default_timezone, i.default_approval_policies,
	i.created_at, i.created_by, i.updated_at, i.updated_by,
	o.observed_at, COALESCE(o.failure,''), COALESCE(o.detail,''), o.reported_installation_uuid, COALESCE(o.reported_package_sha256,''),
	o.reported_migration_generation
	FROM plugin_installations i LEFT JOIN plugin_status_observations o ON o.installation_uuid=i.installation_uuid`

func scanPluginInstallation(row interface{ Scan(...any) error }) (PluginInstallation, error) {
	var item PluginInstallation
	var observedAt sql.NullTime
	var observation PluginStatusObservation
	var reportedInstallation uuid.NullUUID
	var reportedGeneration sql.NullInt64
	var defaultPolicies []byte
	err := row.Scan(&item.InstallationID, &item.PluginID, &item.PackageSHA256, &item.TargetPackageSHA256, &item.State, &item.BackendURL, &item.UIOrigin,
		&item.ServiceUserID, &item.AdmissionCheckerID, &item.OrganizationMode, &item.OrganizationDefaults.Timezone, &defaultPolicies,
		&item.CreatedAt, &item.CreatedBy, &item.UpdatedAt, &item.UpdatedBy,
		&observedAt, &observation.Failure, &observation.Detail, &reportedInstallation, &observation.PackageSHA256, &reportedGeneration)
	if err != nil {
		return item, err
	}
	if err := json.Unmarshal(defaultPolicies, &item.OrganizationDefaults.ApprovalPolicies); err != nil {
		return item, err
	}
	if !observedAt.Valid {
		return item, nil
	}
	observation.ObservedAt = observedAt.Time
	if reportedInstallation.Valid {
		observation.InstallationID = &reportedInstallation.UUID
	}
	if reportedGeneration.Valid {
		observation.MigrationGeneration = &reportedGeneration.Int64
	}
	item.Observation = &observation
	return item, nil
}

// GetPluginInstallation returns an installation with its enablements and
// last status observation. lock takes the installation row for update.
func GetPluginInstallation(ctx context.Context, tx Tx, id uuid.UUID, lock bool) (PluginInstallation, error) {
	query := `SELECT ` + pluginInstallationColumns + ` WHERE i.installation_uuid=$1`
	if lock {
		query += ` FOR UPDATE OF i`
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

// ListPluginInstallations returns every installation with its enablements
// and last status observation.
func ListPluginInstallations(ctx context.Context, tx Tx) ([]PluginInstallation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+pluginInstallationColumns+` ORDER BY i.plugin_id, i.created_at, i.installation_uuid`)
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
	rows, err := tx.QueryContext(ctx, `SELECT e.installation_uuid, e.org_id, e.state, e.timezone, e.approval_policies, e.created_at, e.created_by, e.updated_at, e.updated_by,
			i.package_sha256, COALESCE(r.package_sha256,''), r.reconciled_at, COALESCE(r.failure,'')
		FROM plugin_enablements e JOIN plugin_installations i ON i.installation_uuid=e.installation_uuid
		LEFT JOIN plugin_capability_registrations r ON r.plugin_id=e.plugin_id AND r.org_id=e.org_id
		WHERE $1::uuid IS NULL OR e.installation_uuid=$1 ORDER BY e.org_id`, installation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[uuid.UUID][]PluginEnablement{}
	for rows.Next() {
		var id uuid.UUID
		var item PluginEnablement
		var policies []byte
		var served, failure string
		var reconciledAt sql.NullTime
		registration := &item.CapabilityRegistration
		if err := rows.Scan(&id, &item.OrgID, &item.State, &item.Timezone, &policies, &item.CreatedAt, &item.CreatedBy, &item.UpdatedAt, &item.UpdatedBy,
			&served, &registration.PackageSHA256, &reconciledAt, &failure); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(policies, &item.ApprovalPolicies); err != nil {
			return nil, err
		}
		if reconciledAt.Valid {
			registration.ReconciledAt = &reconciledAt.Time
		}
		switch {
		case registration.PackageSHA256 != served:
			registration.State = PluginCapabilitiesPending
		case failure != "":
			registration.State, registration.Detail = PluginCapabilitiesFailed, failure
		default:
			registration.State = PluginCapabilitiesRegistered
		}
		result[id] = append(result[id], item)
	}
	return result, rows.Err()
}

// LockPluginCapabilities lets one reconciliation of the plugin's IAM
// capabilities at a time proceed until the transaction ends. A reconciler
// that reads what to register after taking the lock never overwrites a newer
// reconciliation with an older package.
func LockPluginCapabilities(ctx context.Context, tx Tx, pluginID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 640))`, "plugin-capabilities:"+pluginID)
	return err
}

// RecordPluginCapabilityReconciliation keeps what Core last did for the
// plugin's capabilities in the organization.
func RecordPluginCapabilityReconciliation(ctx context.Context, tx Tx, reconciliation PluginCapabilityReconciliation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_capability_registrations (plugin_id, org_id, package_sha256, reconciled_at, failure) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (plugin_id, org_id) DO UPDATE SET package_sha256=EXCLUDED.package_sha256, reconciled_at=EXCLUDED.reconciled_at, failure=EXCLUDED.failure`,
		reconciliation.PluginID, reconciliation.OrgID, reconciliation.PackageSHA256, reconciliation.ReconciledAt,
		sql.NullString{String: reconciliation.Failure, Valid: reconciliation.Failure != ""})
	return err
}

// InsertPluginInstallation stores a new installation. Only one installation
// per plugin may exist until it is uninstalled.
func InsertPluginInstallation(ctx context.Context, tx Tx, item PluginInstallation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_installations (installation_uuid, plugin_id, package_sha256, state, backend_url, ui_origin, created_by, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`, item.InstallationID, item.PluginID, item.PackageSHA256, item.State, item.BackendURL,
		sql.NullString{String: item.UIOrigin, Valid: item.UIOrigin != ""}, item.CreatedBy)
	if isUniqueViolation(err, "plugin_installations_one_per_plugin") {
		return ErrConflict{Code: "plugin_installation_exists", Message: "The plugin already has an installation. Resume, upgrade or reinstall that installation, or purge it first."}
	}
	if isUniqueViolation(err, "plugin_installations_pkey") {
		return ErrConflict{Code: "plugin_installation_exists", Message: "The installation UUID is already assigned."}
	}
	return err
}

// UpdatePluginInstallation stores the state, packages, identities and
// organization mode of a locked installation.
func UpdatePluginInstallation(ctx context.Context, tx Tx, item PluginInstallation, actor uuid.UUID) error {
	policies, err := json.Marshal(item.OrganizationDefaults.ApprovalPolicies)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plugin_installations SET state=$2, package_sha256=$3, target_package_sha256=$4, service_user_id=$5,
		admission_checker_id=$6, organization_mode=$7, default_timezone=$8, default_approval_policies=$9, updated_at=now(), updated_by=$10
		WHERE installation_uuid=$1`, item.InstallationID, item.State, item.PackageSHA256,
		sql.NullString{String: item.TargetPackageSHA256, Valid: item.TargetPackageSHA256 != ""},
		sql.NullString{String: item.ServiceUserID, Valid: item.ServiceUserID != ""},
		sql.NullString{String: item.AdmissionCheckerID, Valid: item.AdmissionCheckerID != ""},
		item.OrganizationMode, item.OrganizationDefaults.Timezone, string(policies), actor)
	return err
}

// PluginInstallationsServingAllOrganizations returns the installations in
// organization mode all, in a stable order.
func PluginInstallationsServingAllOrganizations(ctx context.Context, tx Tx) ([]uuid.UUID, error) {
	return pluginInstallationIDs(ctx, tx, `SELECT installation_uuid FROM plugin_installations WHERE organization_mode=$1 ORDER BY installation_uuid`, PluginOrganizationsAll)
}

// PluginInstallationsEnabledFor returns the installations with an
// enablement, in any state, for the organization, in a stable order.
func PluginInstallationsEnabledFor(ctx context.Context, tx Tx, org string) ([]uuid.UUID, error) {
	return pluginInstallationIDs(ctx, tx, `SELECT installation_uuid FROM plugin_enablements WHERE org_id=$1 ORDER BY installation_uuid`, org)
}

func pluginInstallationIDs(ctx context.Context, tx Tx, query string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PluginOrganizationsWithoutEnablement returns the active organizations that
// have no enablement of the plugin, ordered by ID.
func PluginOrganizationsWithoutEnablement(ctx context.Context, tx Tx, pluginID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM orgs o WHERE o.status=$2
		AND NOT EXISTS (SELECT 1 FROM plugin_enablements e WHERE e.plugin_id=$1 AND e.org_id=o.id) ORDER BY o.id`, pluginID, OrgStatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	orgs := []string{}
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, rows.Err()
}

// RecordPluginStatusObservation keeps what Core read from the installation's
// running plugin. An older observation never replaces a newer one.
func RecordPluginStatusObservation(ctx context.Context, db Databaser, id uuid.UUID, observation PluginStatusObservation) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reportedInstallation uuid.NullUUID
	if observation.InstallationID != nil {
		reportedInstallation = uuid.NullUUID{UUID: *observation.InstallationID, Valid: true}
	}
	var reportedGeneration sql.NullInt64
	if observation.MigrationGeneration != nil {
		reportedGeneration = sql.NullInt64{Int64: *observation.MigrationGeneration, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_status_observations
		(installation_uuid, observed_at, failure, detail, reported_installation_uuid, reported_package_sha256, reported_migration_generation)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (installation_uuid) DO UPDATE SET observed_at=EXCLUDED.observed_at, failure=EXCLUDED.failure, detail=EXCLUDED.detail,
			reported_installation_uuid=EXCLUDED.reported_installation_uuid, reported_package_sha256=EXCLUDED.reported_package_sha256,
			reported_migration_generation=EXCLUDED.reported_migration_generation
		WHERE plugin_status_observations.observed_at <= EXCLUDED.observed_at`,
		id, observation.ObservedAt, sql.NullString{String: observation.Failure, Valid: observation.Failure != ""},
		sql.NullString{String: observation.Detail, Valid: observation.Detail != ""}, reportedInstallation,
		sql.NullString{String: observation.PackageSHA256, Valid: observation.PackageSHA256 != ""}, reportedGeneration); err != nil {
		return err
	}
	return tx.Commit()
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

// PluginWork counts the safety work Core still holds for a plugin: operation
// grants not released, dispatches not settled and Module reservations still
// active. Each needs the plugin's cancel, rollback or release path.
type PluginWork struct {
	Grants       int `json:"grants"`
	Dispatches   int `json:"dispatches"`
	Reservations int `json:"reservations"`
}

// Settled reports whether no safety work remains.
func (w PluginWork) Settled() bool {
	return w == PluginWork{}
}

// UnsettledPluginWork counts the plugin's unsettled work in every
// organization. New work needs the installation's rows share-locked by
// RequirePluginEnablement, so a caller holding the installation row for
// update sees every piece of work committed before it and blocks the rest.
func UnsettledPluginWork(ctx context.Context, tx Tx, pluginID string) (PluginWork, error) {
	var work PluginWork
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM external_operation_grants WHERE plugin_id=$1 AND state <> 'released'),
		(SELECT count(*) FROM external_operation_dispatches WHERE plugin_id=$1 AND state <> 'settled'),
		(SELECT count(*) FROM external_module_operations WHERE plugin_id=$1 AND state = 'active')`, pluginID).Scan(&work.Grants, &work.Dispatches, &work.Reservations)
	return work, err
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
