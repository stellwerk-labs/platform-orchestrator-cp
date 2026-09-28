package model

import (
	"context"
	"database/sql"
	"slices"

	"github.com/google/uuid"
)

const maxExternalInventoryItems = 10000

// ExternalInventorySelector is a Core resource selector. It has no Wave,
// progression or approval semantics; the consumer freezes its chosen result.
type ExternalInventorySelector struct {
	EnvironmentIDs   []uuid.UUID       `json:"environment_ids,omitempty"`
	ProjectIDs       []string          `json:"project_ids,omitempty"`
	EnvironmentTypes []string          `json:"environment_types,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
}

func (s ExternalInventorySelector) Matches(environment Environment) bool {
	if len(s.EnvironmentIDs) > 0 && !slices.Contains(s.EnvironmentIDs, environment.Uuid) ||
		len(s.ProjectIDs) > 0 && !slices.Contains(s.ProjectIDs, environment.ProjectId) ||
		len(s.EnvironmentTypes) > 0 && !slices.Contains(s.EnvironmentTypes, environment.EnvTypeId) {
		return false
	}
	for key, value := range s.Labels {
		actual, exists := environment.Labels[key]
		if !exists || actual != value {
			return false
		}
	}
	return true
}

type ExternalInventory struct {
	Environments     []Environment
	EnvironmentTypes []EnvType
	Modules          []ModuleCatalogue
	Versions         []CoreModuleVersion
	Pins             []EnvironmentModuleVersionPin
	PinEvents        []ModuleVersionPinEvent
}

// ReadExternalInventory returns one bounded repeatable-read Core snapshot. The
// API must authorize every selected scope before returning any part of it and
// project versions to metadata, never return embedded definitions or secrets.
func ReadExternalInventory(ctx context.Context, db Databaser, org string, moduleRefs []string, selector ExternalInventorySelector) (*ExternalInventory, error) {
	if len(moduleRefs) > 1000 || len(selector.EnvironmentIDs) > maxExternalInventoryItems || len(selector.ProjectIDs) > 1000 || len(selector.EnvironmentTypes) > 1000 || len(selector.Labels) > 50 {
		return nil, NewErrBadRequest("inventory request exceeds its bounded resource limit")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := db.GetOrg(ctx, tx, org); err != nil {
		return nil, err
	}
	result := &ExternalInventory{}
	for page := ""; ; {
		items, next, err := db.ListEnvironmentsInOrg(ctx, tx, org, page, 200, ListEnvironmentsParams{ByEnvTypeIds: selector.EnvironmentTypes})
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if selector.Matches(item) {
				result.Environments = append(result.Environments, item)
			}
		}
		if len(result.Environments) > maxExternalInventoryItems {
			return nil, NewErrBadRequest("narrow the selector to at most 10000 Environments")
		}
		if next == "" {
			break
		}
		page = next
	}
	for page := ""; ; {
		items, next, err := db.ListEnvironmentTypes(ctx, tx, org, page, 200)
		if err != nil {
			return nil, err
		}
		result.EnvironmentTypes = append(result.EnvironmentTypes, items...)
		if len(result.EnvironmentTypes) > maxExternalInventoryItems {
			return nil, NewErrBadRequest("too many Environment types for one inventory snapshot")
		}
		if next == "" {
			break
		}
		page = next
	}
	selected := map[uuid.UUID]bool{}
	for _, ref := range moduleRefs {
		module, err := db.GetModuleCatalogue(ctx, tx, org, ref, GetModeDefault)
		if err != nil {
			return nil, err
		}
		if selected[module.UUID] {
			return nil, NewErrBadRequest("Module references must resolve to distinct identities")
		}
		selected[module.UUID] = true
		result.Modules = append(result.Modules, *module)
		versions, err := db.ListCoreModuleVersions(ctx, tx, org, module.UUID.String(), true, true)
		if err != nil {
			return nil, err
		}
		result.Versions = append(result.Versions, versions...)
		if len(result.Versions) > maxExternalInventoryItems {
			return nil, NewErrBadRequest("too many Module Versions for one inventory snapshot")
		}
	}
	for _, environment := range result.Environments {
		pins, err := db.ListEnvironmentModuleVersionPins(ctx, tx, org, &environment.Uuid, nil, true)
		if err != nil {
			return nil, err
		}
		for _, pin := range pins {
			if !selected[pin.ModuleUUID] {
				continue
			}
			result.Pins = append(result.Pins, pin)
			events, err := db.ListModuleVersionPinEvents(ctx, tx, org, pin.ID)
			if err != nil {
				return nil, err
			}
			result.PinEvents = append(result.PinEvents, events...)
			if len(result.Pins) > maxExternalInventoryItems || len(result.PinEvents) > maxExternalInventoryItems {
				return nil, NewErrBadRequest("narrow the inventory selector to bound Pin history")
			}
		}
	}
	return result, nil
}

// ListProposedCoreModuleVersions is useful to any version-management client,
// independently of plugin installation or entitlement.
func ListProposedCoreModuleVersions(ctx context.Context, db Databaser, org string) ([]CoreModuleVersion, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+coreModuleVersionColumns+` FROM definition_versions v JOIN definitions d ON d.uuid=v.module_uuid WHERE v.org_id=$1 AND v.semantic_status='proposed' AND d.catalogue_status='active' ORDER BY d.id,v.uuid`, org)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []CoreModuleVersion{}
	for rows.Next() {
		version, err := scanCoreModuleVersion(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *version)
	}
	return result, rows.Err()
}
