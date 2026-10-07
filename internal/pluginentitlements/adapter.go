// Package pluginentitlements connects operator-owned offline evidence to Core's
// transactional entitlement model and plugin availability projection.
package pluginentitlements

import (
	"context"
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

const (
	operatorImporter = "internal_system"
	requiredMode     = "required"
)

var errInstalledBinding = errors.New("installed entitlement binding is invalid")

type installed struct {
	id             string
	installationID string
	manifestSHA256 string
	declaration    plugins.EntitlementDeclaration
	organizations  map[string]plugins.AvailabilityState
}

// Adapter holds only public pinned trust and installed package declarations.
// It never retains an issuer private key or sends raw COSE to a plugin.
type Adapter struct {
	db            model.Databaser
	trust         entitlements.TrustStore
	mode          entitlements.TrustClass
	now           func() time.Time
	installations map[string]installed
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
	Code   string
	Detail string
}

// New loads bounded no-symlink operator files. Both paths may be empty, in
// which case required plugins remain unentitled. Trust-only startup supports
// revalidation of persisted evidence after a key rotation or removal.
//
// The error is reserved for Core configuration: the database handle, the
// entitlement mode and the installed plugin set. Entitlement material (the
// trust file, the bundle and every signed document in it) never fails Core;
// a problem with it is returned as a Diagnostic and only degrades the plugins
// it would have entitled.
func New(ctx context.Context, db model.Databaser, items []plugins.Installation, mode entitlements.TrustClass, trustPath, bundlePath string, clock func() time.Time) (*Adapter, []Diagnostic, error) {
	if db == nil || (mode != entitlements.Production && mode != entitlements.Development) || len(items) > 128 {
		return nil, nil, errors.New("invalid offline entitlement adapter configuration")
	}
	if clock == nil {
		clock = time.Now
	}
	a := &Adapter{db: db, mode: mode, now: clock, trust: entitlements.TrustStore{Class: mode}, installations: make(map[string]installed, len(items))}
	for _, item := range items {
		if item.ID == "" || a.installations[item.ID].id != "" {
			return nil, nil, errors.New("duplicate or empty installed plugin ID")
		}
		saved := installed{id: item.ID, installationID: item.InstallationID, manifestSHA256: item.ManifestSHA256, organizations: map[string]plugins.AvailabilityState{}}
		if item.Entitlement != nil {
			saved.declaration = plugins.EntitlementDeclaration{Mode: item.Entitlement.Mode, Features: slices.Clone(item.Entitlement.Features), Limits: slices.Clone(item.Entitlement.Limits)}
		}
		for org, state := range item.Organizations {
			saved.organizations[org] = state
		}
		a.installations[item.ID] = saved
	}
	var diagnostics []Diagnostic
	if trustPath != "" {
		trust, err := loadTrust(trustPath, mode)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticTrustRejected, err.Error()})
		} else {
			a.trust = trust
		}
	}
	if bundlePath != "" {
		diagnostics = append(diagnostics, a.ImportBundle(ctx, bundlePath)...)
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

// ImportBundle imports every document that verifies on its own signature and,
// for a grant, matches an exact installed binding. A rejected document is
// skipped with a Diagnostic and never blocks the others. Skipping is safe:
// an unverifiable revocation is no revocation, and an unimported grant leaves
// its binding at its persisted, still independently verified decision. Every
// individual import is monotonic and atomic in its own model transaction.
func (a *Adapter) ImportBundle(ctx context.Context, path string) []Diagnostic {
	if a == nil || a.db == nil {
		return []Diagnostic{{DiagnosticBundleRejected, "the entitlement adapter is unavailable"}}
	}
	if len(a.trust.Issuers) == 0 {
		return []Diagnostic{{DiagnosticBundleRejected, "the bundle was not imported because no issuer trust is configured"}}
	}
	raw, err := readRegularFile(path, MaxBundleBytes)
	if err != nil {
		return []Diagnostic{{DiagnosticBundleRejected, fmt.Sprintf("load entitlement bundle: %v", err)}}
	}
	bundle, err := ParseBundleJSON(raw)
	if err != nil {
		return []Diagnostic{{DiagnosticBundleRejected, err.Error()}}
	}
	var candidates []entitlements.Binding
	var diagnostics []Diagnostic
	if len(bundle.Grants) > 0 {
		if candidates, err = a.candidateBindings(ctx); err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticBundleRejected, fmt.Sprintf("no grant was imported: %v", err)})
			bundle.Grants = nil
		}
	}
	grants, snapshots, rejected := verifyBundle(bundle, a.trust, a.mode, candidates, a.now().UTC())
	diagnostics = append(diagnostics, rejected...)
	for _, grant := range grants {
		if _, err := model.ImportPluginEntitlementGrant(ctx, a.db, grant.Grant, operatorImporter, bundle.Digest); err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticGrantRejected, fmt.Sprintf("grant %d was not imported: %v", grant.index, err)})
		}
	}
	for _, snapshot := range snapshots {
		if _, err := model.ImportPluginEntitlementRevocations(ctx, a.db, snapshot.Revocations, operatorImporter, bundle.Digest); err != nil {
			diagnostics = append(diagnostics, Diagnostic{DiagnosticRevocationsRejected, fmt.Sprintf("revocation snapshot %d was not imported: %v", snapshot.index, err)})
		}
	}
	return diagnostics
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

func (a *Adapter) candidateBindings(ctx context.Context) ([]entitlements.Binding, error) {
	candidates := []entitlements.Binding{}
	for _, item := range a.installations {
		if item.declaration.Mode != requiredMode {
			continue
		}
		installationID, err := uuid.Parse(item.installationID)
		if err != nil || installationID == uuid.Nil || installationID.String() != item.installationID {
			return nil, errors.New("invalid installed plugin UUID")
		}
		for org := range item.organizations {
			organization, err := a.db.GetOrg(ctx, nil, org)
			if err != nil {
				return nil, fmt.Errorf("resolve installed organization: %w", err)
			}
			candidates = append(candidates, entitlements.Binding{PluginID: item.id, InstallationID: installationID, OrganizationID: organization.Uuid, DeclaredFeatures: slices.Clone(item.declaration.Features)})
			if len(candidates) > 4096 {
				return nil, errors.New("too many installed entitlement bindings")
			}
		}
	}
	return candidates, nil
}

func (a *Adapter) binding(ctx context.Context, db model.Databaser, tx model.Tx, org string, item plugins.Installation) (entitlements.Binding, plugins.AvailabilityState, error) {
	var binding entitlements.Binding
	if a == nil || db == nil || item.Entitlement == nil || item.Entitlement.Mode != requiredMode {
		return binding, "", fmt.Errorf("%w: required entitlement installation is unavailable", errInstalledBinding)
	}
	configured, ok := a.installations[item.ID]
	if !ok || configured.declaration.Mode != requiredMode || item.InstallationID != configured.installationID || item.ManifestSHA256 != configured.manifestSHA256 || !slices.Equal(item.Entitlement.Features, configured.declaration.Features) || !slices.Equal(item.Entitlement.Limits, configured.declaration.Limits) {
		return binding, "", fmt.Errorf("%w: installed entitlement declaration changed", errInstalledBinding)
	}
	state, ok := configured.organizations[org]
	if !ok || item.Organizations[org] != state {
		return binding, "", fmt.Errorf("%w: plugin is not installed for organization", errInstalledBinding)
	}
	installationID, err := uuid.Parse(configured.installationID)
	if err != nil || installationID == uuid.Nil || installationID.String() != configured.installationID {
		return binding, "", fmt.Errorf("%w: invalid installed plugin UUID", errInstalledBinding)
	}
	organization, err := db.GetOrg(ctx, tx, org)
	if err != nil {
		return binding, "", err
	}
	binding = entitlements.Binding{PluginID: item.ID, InstallationID: installationID, OrganizationID: organization.Uuid, DeclaredFeatures: slices.Clone(configured.declaration.Features)}
	return binding, state, nil
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
	configured, ok := a.installations[item.ID]
	if !ok {
		return status, errors.New("plugin installation is unknown to entitlement adapter")
	}
	if configured.declaration.Mode != requiredMode {
		if item.Entitlement != nil && item.Entitlement.Mode == requiredMode {
			return status, errors.New("installed entitlement declaration changed")
		}
		return status, nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
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
		if decision.Forward && !decision.Read {
			activate := access
			activate.Activate = true
			decision, err = model.DecidePluginEntitlementTx(ctx, a.db, tx, binding, feature, activate, now, a.verifier(binding))
			if err != nil {
				return status, err
			}
		}
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
// committable, allowing observed expiry and evaluated-through to persist.
func (a *Adapter) Guard(ctx context.Context, db model.Databaser, tx model.Tx, org string, item plugins.Installation, feature string, mutating, safety bool) error {
	if a == nil {
		return boundaryError("plugin_not_entitled", "The plugin entitlement adapter is unavailable.")
	}
	configured, ok := a.installations[item.ID]
	if !ok {
		return boundaryError("plugin_not_installed", "The plugin is not installed.")
	}
	if configured.declaration.Mode != requiredMode {
		if item.Entitlement != nil && item.Entitlement.Mode == requiredMode {
			return boundaryError("plugin_not_entitled", "The installed entitlement declaration changed.")
		}
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
