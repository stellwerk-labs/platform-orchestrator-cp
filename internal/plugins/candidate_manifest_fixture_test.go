package plugins

import (
	"os"
	"slices"
	"testing"
)

// Candidate package qualification is opt-in; ordinary Core tests and releases
// do not depend on another repository's private registration artifact.
func TestCandidateObservationRegistrationManifest(t *testing.T) {
	path := os.Getenv("PLUGIN_CANDIDATE_MANIFEST")
	if path == "" {
		t.Skip("set PLUGIN_CANDIDATE_MANIFEST to the candidate registration artifact")
	}
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- opt-in local candidate artifact, never a server request path.
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Operations) != 10 {
		t.Fatalf("candidate operations=%d, want10", len(manifest.Operations))
	}
	if manifest.UI == nil || !manifest.UI.RequireOrgAdmin {
		t.Fatal("candidate UI must require actual organization Admin membership")
	}
	for _, operation := range manifest.Operations {
		if !operation.RequireOrgAdmin {
			t.Fatalf("operation%s lacks strict Admin policy", operation.ID)
		}
	}
	if !slices.Contains(manifest.RequiredProtocols, EnvironmentObservationProtocol) {
		t.Fatal("candidate omitted complete optional Core protocol requirement")
	}
	permission := "plugin." + manifest.ID + ".manage"
	grant, ok := manifest.ServiceGrant("authorization.check")
	if !ok || grant.Permission != permission || grant.Scope != "organization" {
		t.Fatal("authorization check is not bound to the candidate's exact organization manage policy")
	}
	policy, ok := manifest.AuthorizationPolicy(permission)
	if !ok || !slices.Contains(policy.Scopes, "organization") {
		t.Fatal("candidate manage authorization policy does not cover the organization")
	}
	for _, action := range []string{"observation.targets.query", "observation.grant.replace", "observation.grant.revoke", "observation.grant.get", "observation.try-start", "observation.get", "observation.request.get", "observation.redeploy", "observation.redeploy.get"} {
		grant, ok := manifest.ServiceGrant(action)
		if !ok || grant.Permission != permission || grant.Scope != "organization" {
			t.Fatalf("candidate service action%s does not use the exact manage policy", action)
		}
	}
}
