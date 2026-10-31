package genclient

import (
	"encoding/json"
	"testing"
)

func TestPublishedCatalogueRetainsExistingSDKAndWireShape(t *testing.T) {
	// These value-typed fields are part of the published Go SDK contract.
	module := InternalModuleCatalogueModule{SemanticStatus: "default", SemanticVersion: "", MigrationGeneration: "v0", VerificationStatus: "unverified"}
	raw, err := json.Marshal(module)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"semantic_status", "semantic_version", "migration_generation", "verification_status"} {
		if _, found := wire[field]; !found {
			t.Fatalf("legacy published metadata %s disappeared", field)
		}
	}
	if string(wire["semantic_version"]) != `""` {
		t.Fatal("historical empty SemVer must remain empty")
	}
}

func TestExecutionCatalogueDoesNotInventPublishedMetadataForTests(t *testing.T) {
	module := InternalExecutionCatalogueModule{DraftTest: &ModuleDraftTestSnapshot{Provenance: DraftTest}}
	raw, err := json.Marshal(module)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"semantic_status", "semantic_version", "migration_generation", "verification_status"} {
		if _, found := wire[field]; found {
			t.Fatalf("test acquired published metadata %s", field)
		}
	}
}
