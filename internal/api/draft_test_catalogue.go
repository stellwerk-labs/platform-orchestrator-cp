package api

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

// resolveDraftTestCatalogue reads only immutable scoped material. Its caller
// authenticates the DP. Execution authority is checked separately at admission.
func (s *Server) resolveDraftTestCatalogue(ctx context.Context, tx model.Tx, org string, environment *model.Environment, selected map[string]uuid.UUID) (map[string]model.ModuleDefinitionVersion, map[uuid.UUID]ModuleDraftTestSnapshot, error) {
	definitions := make(map[string]model.ModuleDefinitionVersion, len(selected))
	snapshots := make(map[uuid.UUID]ModuleDraftTestSnapshot, len(selected))
	for _, moduleID := range slices.Sorted(maps.Keys(selected)) {
		moduleUUID, err := uuid.Parse(moduleID)
		if err != nil || moduleUUID == uuid.Nil || moduleUUID.String() != moduleID || selected[moduleID] == uuid.Nil {
			return nil, nil, model.NewErrBadRequest("Draft test selections require canonical Module and snapshot UUIDs")
		}
		module, err := s.Database.GetModuleCatalogue(ctx, tx, org, moduleID, model.GetModeDefault)
		if err != nil {
			return nil, nil, err
		}
		snapshot, err := s.Database.GetModuleDraftTestSnapshot(ctx, tx, org, moduleID, environment.Uuid, selected[moduleID])
		if err != nil {
			return nil, nil, err
		}
		if snapshot.ProjectUUID != environment.ProjectUuid {
			return nil, nil, model.NewErrBadRequest("Draft test snapshot belongs to another Project")
		}
		var body ModuleVersionPublishBody
		if err := json.Unmarshal(snapshot.Content, &body); err != nil {
			return nil, nil, err
		}
		definition := publicationModel(module, body, snapshot.CapturedBy)
		definition.VersionUUID, definition.VersionId = snapshot.ID, snapshot.ID.String()
		definition.ResourceTypeVersionUUID, definition.ResourceTypeVersion = snapshot.ResourceTypeVersionUUID, snapshot.ResourceTypeVersion
		definition.CreatedAt, definition.UpdatedAt = snapshot.CapturedAt, snapshot.CapturedAt
		definition.SemanticVersion = ""
		definition.SemanticStatus = ""
		definition.MigrationGeneration = ""
		definition.VerificationStatus = ""
		definition.PublishedBy = nil
		apiSnapshot, err := moduleDraftTestSnapshotToAPI(snapshot)
		if err != nil {
			return nil, nil, err
		}
		definitions[module.Slug] = definition
		snapshots[snapshot.ID] = apiSnapshot
	}
	return definitions, snapshots, nil
}

func (s *Server) resolveRetainedDraftTestCatalogue(ctx context.Context, tx model.Tx, org string, environment *model.Environment, coordinates []ModuleDraftTestCoordinate, existing map[uuid.UUID]ModuleDraftTestSnapshot) ([]model.ModuleDefinitionVersion, error) {
	definitions := make([]model.ModuleDefinitionVersion, 0, len(coordinates))
	for _, coordinate := range coordinates {
		if snapshot, found := existing[coordinate.SnapshotUuid]; found {
			if snapshot.ModuleUuid != coordinate.ModuleUuid {
				return nil, model.NewErrBadRequest("retained snapshot Module identity is inconsistent")
			}
			continue
		}
		selected, snapshots, err := s.resolveDraftTestCatalogue(ctx, tx, org, environment, map[string]uuid.UUID{coordinate.ModuleUuid.String(): coordinate.SnapshotUuid})
		if err != nil {
			return nil, err
		}
		for _, definition := range selected {
			definitions = append(definitions, definition)
		}
		maps.Copy(existing, snapshots)
	}
	return definitions, nil
}

func (s *Server) resolvePublishedCatalogueSelections(ctx context.Context, tx model.Tx, org string, selected map[string]string) (map[string]model.ModuleDefinitionVersion, error) {
	definitions := make(map[string]model.ModuleDefinitionVersion, len(selected))
	for _, module := range slices.Sorted(maps.Keys(selected)) {
		version, err := s.Database.GetCoreModuleVersion(ctx, tx, org, module, selected[module], model.GetModeDefault)
		if err != nil {
			return nil, err
		}
		if previous, found := definitions[version.ModuleSlug]; found && previous.VersionUUID != version.UUID {
			return nil, model.NewErrBadRequest("contradictory published selections for one Module")
		}
		definition, err := s.Database.GetModuleDefinitionVersion(ctx, tx, org, version.ModuleSlug, version.UUID.String())
		if err != nil {
			return nil, err
		}
		definitions[version.ModuleSlug] = *definition
	}
	return definitions, nil
}
