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

// New loads bounded no-symlink operator files. Both paths may be empty, in
// which case required plugins remain unentitled. Trust-only startup supports
// revalidation of persisted evidence after a key rotation or removal.
func New(ctx context.Context, db model.Databaser, items []plugins.Installation, mode entitlements.TrustClass, trustPath, bundlePath string, clock func() time.Time) (*Adapter, error) {
	if db == nil || (mode != entitlements.Production && mode != entitlements.Development) || bundlePath != "" && trustPath == "" || len(items) > 128 {
		return nil, errors.New("invalid offline entitlement adapter configuration")
	}
	if clock == nil {
		clock = time.Now
	}
	a := &Adapter{db: db, mode: mode, now: clock, trust: entitlements.TrustStore{Class: mode}, installations: make(map[string]installed, len(items))}
	for _, item := range items {
		if item.ID == "" || a.installations[item.ID].id != "" {
			return nil, errors.New("duplicate or empty installed plugin ID")
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
	if trustPath != "" {
		raw, err := readRegularFile(trustPath, entitlements.MaxTrustJSONBytes)
		if err != nil {
			return nil, fmt.Errorf("load issuer trust: %w", err)
		}
		a.trust, err = entitlements.ParseTrustJSON(raw, mode)
		if err != nil {
			return nil, fmt.Errorf("parse issuer trust: %w", err)
		}
	}
	if bundlePath != "" {
		if err := a.ImportBundle(ctx, bundlePath); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// ImportBundle verifies every document before writing any of them. Every
// individual import is then monotonic and atomic in the model transaction.
func (a *Adapter) ImportBundle(ctx context.Context, path string) error {
	if a == nil || a.db == nil || len(a.trust.Issuers) == 0 {
		return errors.New("issuer trust is required to import a bundle")
	}
	raw, err := readRegularFile(path, MaxBundleBytes)
	if err != nil {
		return fmt.Errorf("load entitlement bundle: %w", err)
	}
	bundle, err := ParseBundleJSON(raw)
	if err != nil {
		return err
	}
	now := a.now().UTC()
	var candidates []entitlements.Binding
	if len(bundle.Grants) > 0 {
		candidates, err = a.candidateBindings(ctx)
		if err != nil {
			return err
		}
	}
	grants := make([]entitlements.Grant, 0, len(bundle.Grants))
	for _, encoded := range bundle.Grants {
		var verified entitlements.Grant
		matched := false
		for _, candidate := range candidates {
			if verified, err = entitlements.VerifyGrant(encoded, a.trust, a.mode, candidate, now); err == nil {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("signed grant is invalid or has no exact installed binding")
		}
		grants = append(grants, verified)
	}
	snapshots := make([]entitlements.Revocations, 0, len(bundle.Revocations))
	for _, encoded := range bundle.Revocations {
		snapshot, err := entitlements.VerifyRevocations(encoded, a.trust, a.mode, now)
		if err != nil {
			return fmt.Errorf("signed revocation snapshot: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Issuer != grants[j].Issuer {
			return grants[i].Issuer < grants[j].Issuer
		}
		if grants[i].InstallationID != grants[j].InstallationID {
			return grants[i].InstallationID.String() < grants[j].InstallationID.String()
		}
		if grants[i].PluginID != grants[j].PluginID {
			return grants[i].PluginID < grants[j].PluginID
		}
		if grants[i].OrganizationID != grants[j].OrganizationID {
			return grants[i].OrganizationID.String() < grants[j].OrganizationID.String()
		}
		return grants[i].Sequence < grants[j].Sequence
	})
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Issuer != snapshots[j].Issuer {
			return snapshots[i].Issuer < snapshots[j].Issuer
		}
		return snapshots[i].Sequence < snapshots[j].Sequence
	})
	for _, grant := range grants {
		if _, err := model.ImportPluginEntitlementGrant(ctx, a.db, grant, operatorImporter, bundle.Digest); err != nil {
			return fmt.Errorf("import signed grant: %w", err)
		}
	}
	for _, snapshot := range snapshots {
		if _, err := model.ImportPluginEntitlementRevocations(ctx, a.db, snapshot, operatorImporter, bundle.Digest); err != nil {
			return fmt.Errorf("import signed revocations: %w", err)
		}
	}
	return nil
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
// feature activation only after a current valid forward decision, never for an
// expired, revoked, untrusted, or absent grant.
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
	if mutating && !safety && !decision.Forward && (decision.State == string(entitlements.Grace) || decision.State == string(entitlements.Expired) || decision.State == "revoked" || decision.State == "untrusted") {
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
		if state == plugins.StateNotEnabled && decision.State == string(entitlements.Valid) {
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
