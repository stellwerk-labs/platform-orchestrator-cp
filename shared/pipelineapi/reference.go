package pipelineapi

import (
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"strings"
)

// BuiltinReference identifies the unconfigured direct Apply recipe without inventing a published version.
func BuiltinReference() PipelineReference {
	digest, _ := RecipeDigest("Apply", PipelineRecipe{Strategy: Apply, Steps: []PipelineRecipeStep{}})
	name := Default
	return PipelineReference{Builtin: &name, Digest: digest}
}
func ValidateReference(ref PipelineReference) error {
	digest, err := hex.DecodeString(ref.Digest)
	if err != nil || len(digest) != 32 || strings.ToLower(ref.Digest) != ref.Digest {
		return errors.New("pipeline reference requires a lowercase SHA256 digest")
	}
	if (ref.VersionId == nil) == (ref.Builtin == nil) {
		return errors.New("pipeline reference requires exactly one published version or builtin")
	}
	if ref.VersionId != nil && *ref.VersionId == uuid.Nil {
		return errors.New("pipeline version identity is empty")
	}
	if ref.Builtin != nil && (string(*ref.Builtin) != "default" || ref.Digest != BuiltinReference().Digest) {
		return errors.New("builtin pipeline identity changed")
	}
	return nil
}
