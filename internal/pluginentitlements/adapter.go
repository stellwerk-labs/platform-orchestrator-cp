// Package pluginentitlements connects operator-owned offline evidence to Core's
// transactional entitlement model and plugin availability projection.
package pluginentitlements

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/entitlements"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
)

// maxCandidateBindings bounds the installed bindings a bundle is matched against.
const maxCandidateBindings = 4096

var errInstalledBinding = errors.New("installed entitlement binding is invalid")

// Adapter holds only public pinned trust. Installations and their
// organizations come from the Plugin Manager: candidate bindings for an import
// are read in the import transaction, and decisions use the installation the
// caller's snapshot names. It never retains an issuer private key or sends raw
// COSE to a plugin.
type Adapter struct {
	db    model.Databaser
	trust entitlements.TrustStore
	mode  entitlements.TrustClass
	now   func() time.Time
}

var _ plugins.EntitlementResolver = (*Adapter)(nil)

// Diagnostic codes are stable. Each names offline entitlement material that
// Core did not apply. None of them stops Core: an affected plugin keeps its
// persisted decision, which is not_entitled without an imported grant and
// untrusted when a persisted grant no longer verifies against current trust.
const (
	DiagnosticTrustRejected       = "plugin_entitlement_trust_rejected"
	DiagnosticBundleRejected      = "plugin_entitlement_bundle_rejected"
	DiagnosticGrantRejected       = "plugin_entitlement_grant_rejected"
	DiagnosticRevocationsRejected = "plugin_entitlement_revocations_rejected"
)

// Diagnostic is an operator-facing reason, safe to log. Detail never holds
// signed bytes or tenant data beyond what the operator's own files contain.
type Diagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// New loads the bounded no-symlink operator trust file. An empty path leaves
// required plugins unentitled; trust-only startup supports revalidation of
// persisted evidence after a key rotation or removal.
//
// The error is reserved for Core configuration: the database handle and the
// entitlement mode. Trust material never fails Core; a problem with it is
// returned as a Diagnostic and only degrades the plugins it would entitle.
func New(db model.Databaser, mode entitlements.TrustClass, trustPath string, clock func() time.Time) (*Adapter, []Diagnostic, error) {
	if db == nil || (mode != entitlements.Production && mode != entitlements.Development) {
		return nil, nil, errors.New("invalid offline entitlement adapter configuration")
	}
	if clock == nil {
		clock = time.Now
	}
	a := &Adapter{db: db, mode: mode, now: clock, trust: entitlements.TrustStore{Class: mode}}
	var diagnostics []Diagnostic
	if trustPath != "" {
		trust, err := loadTrust(trustPath, mode)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticTrustRejected, err.Error()})
		} else {
			a.trust = trust
		}
	}
	return a, diagnostics, nil
}

func loadTrust(path string, mode entitlements.TrustClass) (entitlements.TrustStore, error) {
	raw, err := readRegularFile(path, entitlements.MaxTrustJSONBytes)
	if err != nil {
		return entitlements.TrustStore{}, fmt.Errorf("load issuer trust: %w", err)
	}
	trust, err := entitlements.ParseTrustJSON(raw, mode)
	if err != nil {
		return entitlements.TrustStore{}, fmt.Errorf("parse issuer trust: %w", err)
	}
	return trust, nil
}

// BundleImport reports what one bundle import changed and why documents were
// skipped.
type BundleImport struct {
	Digest              [32]byte     `json:"-"`
	ImportedGrants      int          `json:"imported_grants"`
	ImportedRevocations int          `json:"imported_revocations"`
	Diagnostics         []Diagnostic `json:"diagnostics"`
}

// ImportBundle imports, in tx, every document that verifies on its own
// signature and, for a grant, matches an exact binding of an installation
// enabled for an organization. A rejected document is skipped with a
// Diagnostic and never blocks the others; each document runs in its own
// savepoint. Skipping is safe: an unverifiable revocation is no revocation,
// and an unimported grant leaves its binding at its persisted, still
// independently verified decision. Every import is monotonic. The error is
// reserved for database failures.
func (a *Adapter) ImportBundle(ctx context.Context, tx model.Tx, raw []byte, importer string) (BundleImport, error) {
	result := BundleImport{Digest: sha256.Sum256(raw), Diagnostics: []Diagnostic{}}
	if a == nil || a.db == nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{DiagnosticBundleRejected, "the entitlement adapter is unavailable"})
		return result, nil
	}
	if len(a.trust.Issuers) == 0 {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{DiagnosticBundleRejected, "the bundle was not imported because no issuer trust is configured"})
		return result, nil
	}
	bundle, err := ParseBundleJSON(raw)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{DiagnosticBundleRejected, err.Error()})
		return result, nil
	}
	var candidates []entitlements.Binding
	if len(bundle.Grants) > 0 {
		if candidates, err = a.candidateBindings(ctx, tx); err != nil {
			return result, err
		}
	}
	grants, snapshots, rejected := verifyBundle(bundle, a.trust, a.mode, candidates, a.now().UTC())
	result.Diagnostics = append(result.Diagnostics, rejected...)
	if len(grants)+len(snapshots) > 0 {
		if err := model.LockPluginEntitlementImportTx(ctx, tx); err != nil {
			return result, err
		}
	}
	for _, grant := range grants {
		imported, err := importDocument(ctx, tx, func() (bool, error) {
			return model.ImportPluginEntitlementGrant(ctx, a.db, tx, grant.Grant, importer, bundle.Digest)
		})
		if err != nil {
			if !isDocumentRejection(err) {
				return result, err
			}
			result.Diagnostics = append(result.Diagnostics, Diagnostic{DiagnosticGrantRejected, fmt.Sprintf("grant %d was not imported: %v", grant.index, err)})
		} else if imported {
			result.ImportedGrants++
		}
	}
	for _, snapshot := range snapshots {
		imported, err := importDocument(ctx, tx, func() (bool, error) {
			return model.ImportPluginEntitlementRevocations(ctx, a.db, tx, snapshot.Revocations, importer, bundle.Digest)
		})
		if err != nil {
			if !isDocumentRejection(err) {
				return result, err
			}
			result.Diagnostics = append(result.Diagnostics, Diagnostic{DiagnosticRevocationsRejected, fmt.Sprintf("revocation snapshot %d was not imported: %v", snapshot.index, err)})
		} else if imported {
			result.ImportedRevocations++
		}
	}
	return result, nil
}

// importDocument confines one document's writes to a savepoint, so a
// rejected document leaves the transaction usable for the others.
func importDocument(ctx context.Context, tx model.Tx, run func() (bool, error)) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT entitlement_document`); err != nil {
		return false, err
	}
	imported, err := run()
	if err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT entitlement_document`); rollbackErr != nil {
			return false, rollbackErr
		}
		return false, err
	}
	_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT entitlement_document`)
	return imported, err
}

// isDocumentRejection reports a refusal of the document itself, as opposed
// to a database failure.
func isDocumentRejection(err error) bool {
	_, conflict := model.IsErrConflict(err)
	_, badRequest := model.IsErrBadRequest(err)
	return conflict || badRequest
}

type indexedGrant struct {
	entitlements.Grant
	index int
}

type indexedRevocations struct {
	entitlements.Revocations
	index int
}

// verifyBundle checks signatures and bindings without writing anything and
// returns the accepted documents in import order. The grant reason stays
// deliberately coarse: it says neither which binding nor which check failed.
func verifyBundle(bundle Bundle, trust entitlements.TrustStore, mode entitlements.TrustClass, candidates []entitlements.Binding, now time.Time) ([]indexedGrant, []indexedRevocations, []Diagnostic) {
	var diagnostics []Diagnostic
	grants := make([]indexedGrant, 0, len(bundle.Grants))
	for i, encoded := range bundle.Grants {
		verified, matched := matchGrant(encoded, trust, mode, candidates, now)
		if !matched {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticGrantRejected, fmt.Sprintf("grant %d is invalid or has no exact installed binding", i)})
			continue
		}
		grants = append(grants, indexedGrant{verified, i})
	}
	snapshots := make([]indexedRevocations, 0, len(bundle.Revocations))
	for i, encoded := range bundle.Revocations {
		snapshot, err := entitlements.VerifyRevocations(encoded, trust, mode, now)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticRevocationsRejected, fmt.Sprintf("revocation snapshot %d: %v", i, err)})
			continue
		}
		snapshots = append(snapshots, indexedRevocations{snapshot, i})
	}
	sort.Slice(grants, func(i, j int) bool {
		left, right := grants[i], grants[j]
		if left.Issuer != right.Issuer {
			return left.Issuer < right.Issuer
		}
		if left.InstallationID != right.InstallationID {
			return left.InstallationID.String() < right.InstallationID.String()
		}
		if left.PluginID != right.PluginID {
			return left.PluginID < right.PluginID
		}
		if left.OrganizationID != right.OrganizationID {
			return left.OrganizationID.String() < right.OrganizationID.String()
		}
		return left.Sequence < right.Sequence
	})
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Issuer != snapshots[j].Issuer {
			return snapshots[i].Issuer < snapshots[j].Issuer
		}
		return snapshots[i].Sequence < snapshots[j].Sequence
	})
	return grants, snapshots, diagnostics
}

func matchGrant(encoded []byte, trust entitlements.TrustStore, mode entitlements.TrustClass, candidates []entitlements.Binding, now time.Time) (entitlements.Grant, bool) {
	for _, candidate := range candidates {
		if verified, err := entitlements.VerifyGrant(encoded, trust, mode, candidate, now); err == nil {
			return verified, true
		}
	}
	return entitlements.Grant{}, false
}

// candidateBindings returns, for every installation Core can serve whose
// package requires an entitlement, the installation scope, which an
// installation-wide grant fits even before any organization is enabled, and
// the binding of each organization enablement.
func (a *Adapter) candidateBindings(ctx context.Context, tx model.Tx) ([]entitlements.Binding, error) {
	installations, err := model.ListPluginInstallations(ctx, tx)
	if err != nil {
		return nil, err
	}
	candidates := []entitlements.Binding{}
	for _, installation := range installations {
		switch installation.State {
		case model.PluginInstallationInstalled, model.PluginInstallationActive, model.PluginInstallationDisabled:
		default:
			continue
		}
		pkg, err := model.GetPluginPackage(ctx, tx, installation.PackageSHA256)
		if err != nil {
			return nil, err
		}
		manifest, err := plugins.DecodeManifest(pkg.Manifest)
		if err != nil {
			return nil, fmt.Errorf("decode installed plugin manifest: %w", err)
		}
		if manifest.Entitlement == nil || manifest.Entitlement.Mode != plugins.EntitlementRequired {
			continue
		}
		candidates = append(candidates, entitlements.Binding{PluginID: installation.PluginID, InstallationID: installation.InstallationID, DeclaredFeatures: slices.Clone(manifest.Entitlement.Features)})
		for _, enablement := range installation.Organizations {
			organization, err := a.db.GetOrg(ctx, tx, enablement.OrgID)
			if err != nil {
				return nil, fmt.Errorf("resolve enabled organization: %w", err)
			}
			candidates = append(candidates, entitlements.Binding{PluginID: installation.PluginID, InstallationID: installation.InstallationID, OrganizationID: organization.Uuid, DeclaredFeatures: slices.Clone(manifest.Entitlement.Features)})
			if len(candidates) > maxCandidateBindings {
				return nil, errors.New("too many installed entitlement bindings")
			}
		}
	}
	return candidates, nil
}

// binding is the signed-grant binding of the installation the caller's
// snapshot names for the organization, with its availability state.
func (a *Adapter) binding(ctx context.Context, db model.Databaser, tx model.Tx, org string, item plugins.Installation) (entitlements.Binding, plugins.AvailabilityState, error) {
	var binding entitlements.Binding
	if a == nil || db == nil || !item.Licensed() {
		return binding, "", fmt.Errorf("%w: required entitlement installation is unavailable", errInstalledBinding)
	}
	state, ok := item.Organizations[org]
	if !ok {
		return binding, "", fmt.Errorf("%w: plugin is not installed for organization", errInstalledBinding)
	}
	installationID, err := uuid.Parse(item.InstallationID)
	if err != nil || installationID == uuid.Nil || installationID.String() != item.InstallationID {
		return binding, "", fmt.Errorf("%w: invalid installed plugin UUID", errInstalledBinding)
	}
	organization, err := db.GetOrg(ctx, tx, org)
	if err != nil {
		return binding, "", err
	}
	binding = entitlements.Binding{PluginID: item.ID, InstallationID: installationID, OrganizationID: organization.Uuid, DeclaredFeatures: slices.Clone(item.Entitlement.Features)}
	return binding, state, nil
}

// GrantStatuses returns the current grant of every binding of the
// installation with its state under current trust, keyed by organization ID,
// the installation-wide grant under "". declared are the features of the
// served package.
func (a *Adapter) GrantStatuses(ctx context.Context, tx model.Tx, installation uuid.UUID, plugin string, declared []string) (map[string]model.PluginEntitlementGrantStatus, error) {
	return model.PluginEntitlementGrantStatuses(ctx, tx, installation, plugin, a.now(), func(raw []byte, binding entitlements.Binding, now time.Time) (entitlements.Grant, error) {
		binding.DeclaredFeatures = declared
		return entitlements.VerifyGrant(raw, a.trust, a.mode, binding, now)
	})
}

func (a *Adapter) verifier(binding entitlements.Binding) model.PluginGrantVerifier {
	return func(raw []byte, effectiveNow time.Time) (entitlements.Grant, error) {
		return entitlements.VerifyGrant(raw, a.trust, a.mode, binding, effectiveNow)
	}
}

func baseAccess(state plugins.AvailabilityState) model.PluginEntitlementAccess {
	compatible := state != plugins.StateIncompatible && state != plugins.StateUnavailable
	retained := state == plugins.StateAvailable || state == plugins.StateNotEnabled || state == plugins.StateNotEntitled
	return model.PluginEntitlementAccess{Enabled: state == plugins.StateAvailable, Retained: retained, Compatible: compatible}
}

// ResolveEntitlement is a discovery projection. It creates a first durable
// feature activation only after a current forward decision (valid or grace),
// never for an expired, revoked, untrusted, or absent grant.
func (a *Adapter) ResolveEntitlement(ctx context.Context, org string, item plugins.Installation) (plugins.EntitlementStatus, error) {
	status := plugins.EntitlementStatus{Features: map[string]plugins.FeatureAccess{}}
	if a == nil || a.db == nil {
		return status, errors.New("entitlement adapter is unavailable")
	}
	if !item.Licensed() {
		return status, nil
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return status, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, state, err := a.binding(ctx, a.db, tx, org, item)
	if err != nil {
		return status, err
	}
	access := baseAccess(state)
	now := a.now().UTC()
	for _, feature := range binding.DeclaredFeatures {
		decision, err := model.DecidePluginEntitlementTx(ctx, a.db, tx, binding, feature, access, now, a.verifier(binding))
		if err != nil {
			return status, err
		}
		// The first activation is a Core effect: only an installation that is
		// still active and enabled in the database may cause it.
		if decision.Forward && !decision.Read && enabled(ctx, tx, org, item) {
			activate := access
			activate.Activate = true
			decision, err = model.DecidePluginEntitlementTx(ctx, a.db, tx, binding, feature, activate, now, a.verifier(binding))
			if err != nil {
				return status, err
			}
		}
		status.GrantScope = decision.Scope
		class := string(decision.TrustClass)
		if class == "" {
			class = string(a.mode)
		}
		status.Features[feature] = plugins.FeatureAccess{State: decision.State, TrustClass: class, Forward: decision.Forward, Read: decision.Read, Safety: decision.Safety, ExpiresAt: decision.ExpiresAt, GraceUntil: decision.GraceUntil}
	}
	if err := tx.Commit(); err != nil {
		return plugins.EntitlementStatus{}, err
	}
	return status, nil
}

// Guard uses the caller's effect transaction and leaves a denied transaction
// committable, allowing observed expiry and evaluated-through to persist. The
// caller has already re-verified the installation and enablement in tx.
func (a *Adapter) Guard(ctx context.Context, db model.Databaser, tx model.Tx, org string, item plugins.Installation, feature string, mutating, safety bool) error {
	if a == nil {
		return boundaryError("plugin_not_entitled", "The plugin entitlement adapter is unavailable.")
	}
	if !item.Licensed() {
		return nil
	}
	if tx == nil || db == nil {
		return errors.New("entitlement guard requires caller transaction")
	}
	binding, state, err := a.binding(ctx, db, tx, org, item)
	if err != nil {
		if _, missing := model.IsErrNotFound(err); missing || errors.Is(err, errInstalledBinding) {
			return boundaryError("plugin_not_entitled", "The installed plugin entitlement could not be matched.")
		}
		return err
	}
	if !slices.Contains(binding.DeclaredFeatures, feature) {
		return boundaryError("plugin_not_entitled", "The operation requires an undeclared plugin feature.")
	}
	access := baseAccess(state)
	now := a.now().UTC()
	decision, err := model.DecidePluginEntitlementTx(ctx, db, tx, binding, feature, access, now, a.verifier(binding))
	if err != nil {
		return err
	}
	if mutating && !safety && !decision.Forward && (decision.State == string(entitlements.Expired) || decision.State == "revoked" || decision.State == "untrusted") {
		return boundaryError("plugin_not_entitled", "The plugin feature has no current entitlement for this operation.")
	}
	if state == plugins.StateIncompatible {
		return boundaryError("plugin_incompatible", "The plugin installation is incompatible.")
	}
	if state == plugins.StateUnavailable {
		return boundaryError("plugin_unavailable", "The plugin installation is unavailable.")
	}
	permitted := decision.Read
	if mutating && safety {
		permitted = decision.Safety
	}
	if mutating && !safety {
		permitted = decision.Forward
	}
	if !permitted {
		if state == plugins.StateNotEnabled && (decision.State == string(entitlements.Valid) || decision.State == string(entitlements.Grace)) {
			return boundaryError("plugin_not_enabled", "The plugin is not enabled for this organization.")
		}
		return boundaryError("plugin_not_entitled", "The plugin feature has no current entitlement for this operation.")
	}
	if mutating && !safety {
		activate := access
		activate.Activate = true
		if _, err := model.DecidePluginEntitlementTx(ctx, db, tx, binding, feature, activate, now, a.verifier(binding)); err != nil {
			return err
		}
	}
	return nil
}

// enabled re-verifies in tx that the installation is active and enabled for
// the organization. A stale snapshot or a lookup failure counts as disabled.
func enabled(ctx context.Context, tx model.Tx, org string, item plugins.Installation) bool {
	return model.RequirePluginEnablement(ctx, tx, model.PluginUse{OrgID: org, PluginID: item.ID, InstallationID: item.InstallationID, PackageSHA256: item.ManifestSHA256}) == nil
}

func boundaryError(code, message string) error {
	return &herrors.PlatformOrchestratorError{StatusCode: 409, Code: code, Message: message}
}

// Sweep records observed grace and expiry boundaries even without requests.
// The caller schedules this bounded, local database operation.
func (a *Adapter) Sweep(ctx context.Context, limit int) (int, error) {
	if a == nil || a.db == nil {
		return 0, errors.New("entitlement adapter is unavailable")
	}
	return model.SweepPluginEntitlementBoundaries(ctx, a.db, a.now().UTC(), limit)
}
