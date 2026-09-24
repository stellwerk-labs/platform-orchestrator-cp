package moduleversions

import (
	"fmt"
	"regexp"
)

var artifactDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidateArtifactDigest validates the canonical digest of an external Module
// artifact. Inline Module source is already contained by the immutable Module
// Version and therefore has no external artifact digest.
func ValidateArtifactDigest(value string) error {
	if !artifactDigestPattern.MatchString(value) {
		return fmt.Errorf("artifact digest must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	return nil
}
