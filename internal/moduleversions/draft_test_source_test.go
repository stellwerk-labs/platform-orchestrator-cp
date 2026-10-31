package moduleversions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDraftTestSourceRequiresExecutablePin(t *testing.T) {
	commit := strings.Repeat("a", 40)
	code := "output \"name\" { value = \"test\" }"
	wrong := strings.Repeat("b", 40)
	cases := []struct {
		name, source           string
		code, revision, digest *string
		valid                  bool
	}{
		{name: "inline", source: "inline", code: &code, valid: true},
		{name: "empty inline", source: "inline"},
		{name: "inline digest", source: "inline", code: &code, digest: &commit},
		{name: "https exact commit", source: "git::https://example.invalid/modules.git//storage?ref=" + commit, revision: &commit, valid: true},
		{name: "ssh exact commit", source: "git::ssh://git@example.invalid/modules.git?ref=" + commit, revision: &commit, valid: true},
		{name: "revision label cannot freeze branch", source: "git::https://example.invalid/modules.git?ref=main", revision: &commit},
		{name: "no selector", source: "git::https://example.invalid/modules.git", revision: &commit},
		{name: "mismatched revision", source: "git::https://example.invalid/modules.git?ref=" + commit, revision: &wrong},
		{name: "missing revision", source: "git::https://example.invalid/modules.git?ref=" + commit},
		{name: "duplicate selector", source: "git::https://example.invalid/modules.git?ref=" + commit + "&ref=" + commit, revision: &commit},
		{name: "shallow commit is not a branch", source: "git::https://example.invalid/modules.git?ref=" + commit + "&depth=1", revision: &commit},
		{name: "fragment", source: "git::https://example.invalid/modules.git?ref=" + commit + "#main", revision: &commit},
		{name: "local source", source: "/tmp/moving-module", revision: &commit},
		{name: "mutable archive", source: "https://example.invalid/module.zip", revision: &commit},
		{name: "unsupported transport", source: "git::file:///tmp/repo?ref=" + commit, revision: &commit},
		{name: "embedded password", source: "git::https://user:password@example.invalid/repo?ref=" + commit, revision: &commit},
		{name: "ambiguous source content", source: "git::https://example.invalid/repo?ref=" + commit, revision: &commit, code: &code},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDraftTestSource(tt.source, tt.code, tt.revision, tt.digest)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
