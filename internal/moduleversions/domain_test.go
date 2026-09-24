package moduleversions

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mustVersion(t *testing.T, value string) Version {
	t.Helper()
	version, err := ParseVersion(value)
	require.NoError(t, err)
	return version
}

func TestCanonicalSemVerAndPrecedence(t *testing.T) {
	for _, value := range []string{"1.0.0", "2.3.4-alpha.1", "2.3.4+build.9"} {
		require.Equal(t, value, mustVersion(t, value).String())
	}
	for _, value := range []string{"v1.0.0", "01.0.0", "1.0", "1.0.0-01", " 1.0.0"} {
		_, err := ParseVersion(value)
		require.Error(t, err)
	}
	require.Negative(t, mustVersion(t, "1.0.0-rc.1").Compare(mustVersion(t, "1.0.0")))
	require.Zero(t, mustVersion(t, "1.0.0+one").Compare(mustVersion(t, "1.0.0+two")))
}

func TestPublicationMustAdvanceDefault(t *testing.T) {
	current := mustVersion(t, "2.4.0")
	require.NoError(t, ValidatePublicationVersion(mustVersion(t, "2.5.0-alpha.1"), &current))
	require.Error(t, ValidatePublicationVersion(mustVersion(t, "2.4.0"), &current))
	require.Error(t, ValidatePublicationVersion(mustVersion(t, "2.3.9"), &current))
}

func TestLifecycleTransitions(t *testing.T) {
	stable := mustVersion(t, "2.0.0")
	prerelease := mustVersion(t, "2.1.0-rc.1")
	require.NoError(t, ValidateLifecycleTransition(LifecycleProposed, LifecycleDefault, stable))
	require.NoError(t, ValidateLifecycleTransition(LifecycleDefault, LifecycleDefective, stable))
	require.NoError(t, ValidateLifecycleTransition(LifecycleDeprecated, LifecycleDefault, stable))
	require.Error(t, ValidateLifecycleTransition(LifecycleProposed, LifecycleDefault, prerelease))
	require.Error(t, ValidateLifecycleTransition(LifecycleDefective, LifecycleDefault, stable))
}

func TestPinStateMachineIsOperationLockedAndRemovedIsTerminal(t *testing.T) {
	require.NoError(t, ValidatePinTransition(PinActive, PinOverridePending))
	require.NoError(t, ValidatePinTransition(PinOverridePending, PinOverridden))
	require.NoError(t, ValidatePinTransition(PinOverridePending, PinActive))
	require.NoError(t, ValidatePinTransition(PinOverridden, PinActive))
	require.Error(t, ValidatePinTransition(PinOverridePending, PinRemoved))
	require.Error(t, ValidatePinTransition(PinRemoved, PinActive))
}
