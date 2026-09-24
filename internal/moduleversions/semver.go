package moduleversions

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var canonicalSemVerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

type Version struct {
	Major      uint64
	Minor      uint64
	Patch      uint64
	PreRelease []string
	Build      []string
	raw        string
}

func ParseVersion(value string) (Version, error) {
	matches := canonicalSemVerPattern.FindStringSubmatch(value)
	if matches == nil {
		return Version{}, fmt.Errorf("%q is not canonical Semantic Versioning", value)
	}
	parts := make([]uint64, 3)
	for index := range parts {
		parsed, err := strconv.ParseUint(matches[index+1], 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("invalid semantic version %q: %w", value, err)
		}
		parts[index] = parsed
	}
	version := Version{Major: parts[0], Minor: parts[1], Patch: parts[2], raw: value}
	if matches[4] != "" {
		version.PreRelease = strings.Split(matches[4], ".")
	}
	if matches[5] != "" {
		version.Build = strings.Split(matches[5], ".")
	}
	return version, nil
}

func (v Version) String() string { return v.raw }

func (v Version) Compare(other Version) int {
	for _, pair := range [][2]uint64{{v.Major, other.Major}, {v.Minor, other.Minor}, {v.Patch, other.Patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(v.PreRelease) == 0 && len(other.PreRelease) == 0 {
		return 0
	}
	if len(v.PreRelease) == 0 {
		return 1
	}
	if len(other.PreRelease) == 0 {
		return -1
	}
	for index := 0; index < len(v.PreRelease) && index < len(other.PreRelease); index++ {
		left, right := v.PreRelease[index], other.PreRelease[index]
		leftNumber, leftErr := strconv.ParseUint(left, 10, 64)
		rightNumber, rightErr := strconv.ParseUint(right, 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	if len(v.PreRelease) < len(other.PreRelease) {
		return -1
	}
	if len(v.PreRelease) > len(other.PreRelease) {
		return 1
	}
	return 0
}
