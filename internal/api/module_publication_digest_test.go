package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"
)

func TestPublicationOptionalArtifactDigestDoesNotRelaxSuppliedClaims(t *testing.T) {
	canonical := "sha256:" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name   string
		digest *string
		inline bool
		valid  bool
	}{
		{name: "external omitted", valid: true},
		{name: "external canonical", digest: &canonical, valid: true},
		{name: "external explicit empty", digest: ref.Ref("")},
		{name: "external uppercase", digest: ref.Ref(strings.ToUpper(canonical))},
		{name: "external truncated", digest: ref.Ref("sha256:abc")},
		{name: "external unsupported algorithm", digest: ref.Ref("sha512:" + strings.Repeat("a", 128))},
		{name: "inline omitted", inline: true, valid: true},
		{name: "inline empty forbidden", inline: true, digest: ref.Ref("")},
		{name: "inline canonical forbidden", inline: true, digest: &canonical},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := ModuleVersionPublishBody{
				SemanticVersion: "1.0.0", ModuleSource: "git::https://example.invalid/module.git?ref=release-one",
				SourceRevision: ref.Ref("release-one"), ArtifactDigest: test.digest,
			}
			if test.inline {
				body.ModuleSource = "inline"
				body.ModuleSourceCode = ref.Ref(`output "value" { value = "release-one" }`)
				body.SourceRevision = nil
			}
			err := validatePublicationBody(body)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	withoutRevision := ModuleVersionPublishBody{
		SemanticVersion: "1.0.0", ModuleSource: "git::https://example.invalid/module.git?ref=release-one",
	}
	require.ErrorContains(t, validatePublicationBody(withoutRevision), "source_revision is required")
}
