package moduleversions

import (
	"fmt"
	"strings"
)

type LifecycleStatus string

const (
	LifecycleProposed   LifecycleStatus = "proposed"
	LifecycleDefault    LifecycleStatus = "default"
	LifecycleDeprecated LifecycleStatus = "deprecated"
	LifecycleDefective  LifecycleStatus = "defective"
)

type VerificationStatus string

const VerificationUnverified VerificationStatus = "unverified"

type CatalogueStatus string

const (
	CatalogueActive   CatalogueStatus = "active"
	CatalogueArchived CatalogueStatus = "archived"
)

type PinStatus string

const (
	PinActive          PinStatus = "active"
	PinOverridePending PinStatus = "override_pending"
	PinOverridden      PinStatus = "overridden"
	PinRemoved         PinStatus = "removed"
)

func ValidatePublicationVersion(candidate Version, currentDefault *Version) error {
	if currentDefault != nil && candidate.Compare(*currentDefault) <= 0 {
		return fmt.Errorf("published version %s must have higher precedence than the current default %s", candidate, *currentDefault)
	}
	return nil
}

func ValidateLifecycleTransition(from, to LifecycleStatus, version Version) error {
	if from == to {
		return nil
	}
	if to == LifecycleDefault && len(version.PreRelease) != 0 {
		return fmt.Errorf("prerelease version %s cannot become default", version)
	}
	allowed := map[LifecycleStatus]map[LifecycleStatus]bool{
		LifecycleProposed: {
			LifecycleDefault:    true,
			LifecycleDeprecated: true,
			LifecycleDefective:  true,
		},
		LifecycleDefault: {
			LifecycleDeprecated: true,
			LifecycleDefective:  true,
		},
		LifecycleDeprecated: {
			LifecycleDefault:   true,
			LifecycleDefective: true,
		},
		LifecycleDefective: {},
	}
	if !allowed[from][to] {
		return fmt.Errorf("invalid module version lifecycle transition %s -> %s", from, to)
	}
	return nil
}

func RequireTransitionReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("a human-readable reason is required")
	}
	return nil
}

func ValidatePinTransition(from, to PinStatus) error {
	if from == to {
		return nil
	}
	allowed := map[PinStatus]map[PinStatus]bool{
		PinActive: {
			PinOverridePending: true,
			PinRemoved:         true,
		},
		PinOverridePending: {
			PinActive:     true,
			PinOverridden: true,
		},
		PinOverridden: {
			PinActive:  true,
			PinRemoved: true,
		},
		PinRemoved: {},
	}
	if !allowed[from][to] {
		return fmt.Errorf("invalid module version pin transition %s -> %s", from, to)
	}
	return nil
}
