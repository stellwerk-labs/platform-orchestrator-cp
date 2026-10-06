package model

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExternalOperationInventoryScopeIsExactlyTheFrozenIntent(t *testing.T) {
	moduleA, moduleB := uuid.New(), uuid.New()
	inside, outside := uuid.New(), uuid.New()
	grant := ExternalOperationGrant{Intent: ExternalOperationIntent{
		Modules:      []uuid.UUID{moduleA, moduleB},
		Environments: []ExternalOperationEnvironment{{ProjectID: uuid.New(), EnvironmentID: inside}},
	}}
	modules, inScope := externalOperationInventoryScope(grant)
	require.Equal(t, []string{moduleA.String(), moduleB.String()}, modules, "Modules are addressed by their immutable UUID")
	require.True(t, inScope(Environment{Uuid: inside}))
	require.False(t, inScope(Environment{Uuid: outside}))

	grant.Intent.Environments = nil
	_, inScope = externalOperationInventoryScope(grant)
	require.False(t, inScope(Environment{Uuid: inside}), "an operation without Environments sees none, unlike an empty selector")
	require.True(t, ExternalInventorySelector{}.Matches(Environment{Uuid: inside}), "the delegated selector keeps its meaning")
}

func TestExternalOperationReconcilesLikeTheJournalWindow(t *testing.T) {
	now, window := time.Now(), 30*24*time.Hour
	for name, tc := range map[string]struct {
		state      string
		released   time.Time
		reconciles bool
	}{
		"active":                      {grantStateActive, now.Add(-365 * 24 * time.Hour), true},
		"suspended":                   {grantStateSuspended, now.Add(-365 * 24 * time.Hour), true},
		"released within the window":  {grantStateReleased, now.Add(-window + time.Hour), true},
		"released before the window":  {grantStateReleased, now.Add(-window - time.Hour), false},
		"prepared has not started":    {grantStatePrepared, now, false},
		"an unknown state is refused": {"archived", now, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.reconciles, externalOperationReconcilesAt(tc.state, tc.released, now, window))
		})
	}
}
