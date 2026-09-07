package plugins

import (
	"context"
	"slices"
)

// AvailabilityState is the server-authoritative state exposed to clients during
// capability discovery. Installation, enablement, entitlement, compatibility,
// and runtime health deliberately remain distinct so clients can give an
// actionable error instead of reporting an undifferentiated Core failure.
type AvailabilityState string

const (
	StateAvailable    AvailabilityState = "available"
	StateNotInstalled AvailabilityState = "not_installed"
	StateNotEnabled   AvailabilityState = "not_enabled"
	StateNotEntitled  AvailabilityState = "not_entitled"
	StateIncompatible AvailabilityState = "incompatible"
	StateUnavailable  AvailabilityState = "unavailable"
)

type Descriptor struct {
	ID           string
	DisplayName  string
	Version      string
	Capabilities []string
	State        AvailabilityState
	AdminAction  string
}

type Registry interface {
	Get(ctx context.Context, orgID, pluginID string) (Descriptor, bool)
}

type StaticRegistry struct {
	plugins map[string]Descriptor
}

func NewStaticRegistry(descriptors ...Descriptor) *StaticRegistry {
	registry := &StaticRegistry{plugins: make(map[string]Descriptor, len(descriptors))}
	for _, descriptor := range descriptors {
		descriptor.Capabilities = slices.Clone(descriptor.Capabilities)
		registry.plugins[descriptor.ID] = descriptor
	}
	return registry
}

func (r *StaticRegistry) Get(_ context.Context, _, pluginID string) (Descriptor, bool) {
	if r == nil {
		return Descriptor{}, false
	}
	descriptor, ok := r.plugins[pluginID]
	descriptor.Capabilities = slices.Clone(descriptor.Capabilities)
	return descriptor, ok
}
