package pipelineapi

import (
	"github.com/google/uuid"
	"testing"
)

func TestPipelineReferenceHasOneImmutableIdentity(t *testing.T) {
	builtin := BuiltinReference()
	if err := ValidateReference(builtin); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	builtin.VersionId = &id
	if ValidateReference(builtin) == nil {
		t.Fatal("mixed builtin/published identity accepted")
	}
	builtin.VersionId = nil
	builtin.Digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if ValidateReference(builtin) == nil {
		t.Fatal("changed builtin definition accepted")
	}
}
