package plugins

import (
	"context"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EntitlementRequired is the declaration mode of a licensed plugin.
const EntitlementRequired = "required"

// EntitlementDeclaration is copied from the verified package manifest by the
// installer. It declares requirements, never grants licence or IAM authority.
type EntitlementDeclaration struct {
	Mode     string   `json:"mode"`
	Features []string `json:"features"`
	Limits   []string `json:"limits"`
}

type FeatureAccess struct {
	State      string     `json:"state"`
	TrustClass string     `json:"trust_class"`
	Forward    bool       `json:"forward"`
	Read       bool       `json:"read"`
	Safety     bool       `json:"safety"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	GraceUntil *time.Time `json:"grace_until,omitempty"`
}

type EntitlementStatus struct {
	// GrantScope names the grant that covers the organization: its own
	// organization grant or the installation-wide grant. Empty without one.
	GrantScope string                   `json:"grant_scope,omitempty"`
	Features   map[string]FeatureAccess `json:"features"`
	Warning    *EntitlementWarning      `json:"warning,omitempty"`
}

// EntitlementGraceWarning is the stable code clients key on. The message is
// display text and may change.
const EntitlementGraceWarning = "plugin_entitlement_grace"

// EntitlementWarning tells clients that forward work still succeeds while a
// signed licence is in its grace period. It never changes authorization.
type EntitlementWarning struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	GraceUntil time.Time `json:"grace_until"`
	Features   []string  `json:"features"`
}

// GraceWarning returns the warning for the given features that are in their
// grace period, or nil when none is. GraceUntil is the earliest end.
func (s *EntitlementStatus) GraceWarning(features ...string) *EntitlementWarning {
	if s == nil {
		return nil
	}
	var warning *EntitlementWarning
	for _, feature := range features {
		access := s.Features[feature]
		if access.State != "grace" || access.GraceUntil == nil {
			continue
		}
		if warning == nil {
			warning = &EntitlementWarning{Code: EntitlementGraceWarning, GraceUntil: access.GraceUntil.UTC()}
		}
		if access.GraceUntil.Before(warning.GraceUntil) {
			warning.GraceUntil = access.GraceUntil.UTC()
		}
		warning.Features = append(warning.Features, feature)
	}
	if warning != nil {
		slices.Sort(warning.Features)
		warning.Features = slices.Compact(warning.Features)
		warning.Message = "The signed plugin licence is in its grace period until " + warning.GraceUntil.Format(time.RFC3339) + ". Forward work continues until then; ask an administrator to renew the licence."
	}
	return warning
}

// EntitlementResolver evaluates persisted, verified grants against current
// issuer trust. Effectful operations must additionally check in their own Core
// transaction; this discovery projection is not an execution credential.
type EntitlementResolver interface {
	ResolveEntitlement(context.Context, string, Installation) (EntitlementStatus, error)
}

var featureSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// validateEntitlement checks the declaration and the binding Core uses for
// signed grants.
func validateEntitlement(item Installation) error {
	if err := validateEntitlementDeclaration(item.Manifest()); err != nil {
		return err
	}
	return validateEntitlementBinding(item)
}

func validateEntitlementDeclaration(m Manifest) error {
	declaration := m.Entitlement
	if declaration == nil || declaration.Mode == "none" {
		if declaration != nil && (len(declaration.Features) != 0 || len(declaration.Limits) != 0) {
			return fmt.Errorf("unlicensed plugin cannot declare entitlement features or limits")
		}
		for _, op := range m.Operations {
			if op.RequiredFeature != "" {
				return fmt.Errorf("operation requires an undeclared entitlement feature")
			}
		}
		for _, grant := range m.ServiceGrants {
			if grant.RequiredFeature != "" {
				return fmt.Errorf("service action requires an undeclared entitlement feature")
			}
		}
		return nil
	}
	if declaration.Mode != EntitlementRequired || len(declaration.Features) == 0 || len(declaration.Features) > 64 || len(declaration.Limits) != 0 {
		return fmt.Errorf("entitlement requires bounded declared features and no unsupported limits")
	}
	for i, feature := range declaration.Features {
		if !featureSlug.MatchString(feature) || i > 0 && declaration.Features[i-1] >= feature {
			return fmt.Errorf("entitlement features must be sorted unique local slugs")
		}
	}
	for _, op := range m.Operations {
		if !slices.Contains(declaration.Features, op.RequiredFeature) {
			return fmt.Errorf("every licensed operation requires a declared feature")
		}
	}
	for _, grant := range m.ServiceGrants {
		if !slices.Contains(declaration.Features, grant.RequiredFeature) {
			return fmt.Errorf("every licensed service action requires a declared feature")
		}
	}
	return nil
}

// Licensed reports whether the package requires a signed entitlement.
func (item Installation) Licensed() bool {
	return item.Entitlement != nil && item.Entitlement.Mode == EntitlementRequired
}

// validateEntitlementBinding requires the identifiers a signed grant binds to:
// the Core-assigned installation UUID and the package manifest digest.
func validateEntitlementBinding(item Installation) error {
	if !item.Licensed() {
		return nil
	}
	id, err := uuid.Parse(item.InstallationID)
	if err != nil || id == uuid.Nil || id.String() != item.InstallationID {
		return fmt.Errorf("entitlement requires an immutable installation UUID")
	}
	digest, err := hex.DecodeString(item.ManifestSHA256)
	if err != nil || len(digest) != 32 || strings.ToLower(item.ManifestSHA256) != item.ManifestSHA256 {
		return fmt.Errorf("entitlement requires the verified package manifest SHA-256")
	}
	return nil
}

func (d Descriptor) Allows(feature string, mutating, safety bool) bool {
	if d.State != StateAvailable && d.State != StateNotEnabled && d.State != StateNotEntitled {
		return false
	}
	if d.Entitlement == nil {
		return d.State == StateAvailable || !mutating || safety
	}
	access, found := d.Entitlement.Features[feature]
	if !found {
		return false
	}
	if !mutating {
		return access.Read
	}
	if safety {
		return access.Safety
	}
	return d.State == StateAvailable && access.Forward
}

func (r *InstallationRegistry) applyEntitlement(ctx context.Context, org string, item Installation, descriptor Descriptor) Descriptor {
	if item.Entitlement == nil || item.Entitlement.Mode == "none" {
		return descriptor
	}
	status := EntitlementStatus{Features: map[string]FeatureAccess{}}
	if r.Entitlements != nil {
		var err error
		status, err = r.Entitlements.ResolveEntitlement(ctx, org, item)
		if err != nil {
			descriptor.State, descriptor.Mode = StateUnavailable, ModeHidden
			descriptor.AdminAction = "The plugin licence could not be evaluated. Ask an administrator to check the entitlement service."
			descriptor.Entitlement = &EntitlementStatus{Features: map[string]FeatureAccess{}}
			return descriptor
		}
	}
	return projectEntitlement(item, descriptor, status)
}

func projectEntitlement(item Installation, descriptor Descriptor, status EntitlementStatus) Descriptor {
	// A resolver cannot advertise a feature that was not in the signed manifest.
	filtered := EntitlementStatus{GrantScope: status.GrantScope, Features: map[string]FeatureAccess{}}
	forward, retained := false, false
	for _, feature := range item.Entitlement.Features {
		access, found := status.Features[feature]
		if !found {
			access.State = "missing"
		}
		filtered.Features[feature] = access
		forward = forward || access.Forward
		retained = retained || access.Read || access.Safety
	}
	filtered.Warning = filtered.GraceWarning(item.Entitlement.Features...)
	descriptor.Entitlement = &filtered
	if descriptor.State == StateIncompatible || descriptor.State == StateUnavailable {
		return descriptor
	}
	if descriptor.State == StateAvailable && forward {
		descriptor.Mode = ModeFull
		if filtered.Warning != nil {
			descriptor.AdminAction = filtered.Warning.Message
		}
		return descriptor
	}
	if descriptor.State == StateAvailable {
		descriptor.State = StateNotEntitled
	}
	descriptor.Mode = ModeHidden
	if retained {
		descriptor.Mode = ModeSafetyOnly
	}
	descriptor.AdminAction = "Ask an administrator to install or renew a signed licence for this plugin and organization."
	return descriptor
}
