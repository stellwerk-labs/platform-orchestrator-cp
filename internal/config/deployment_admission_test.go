package config

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

func TestDeploymentAdmissionIdentitiesAreExplicitAndCanonical(t *testing.T) {
	id := userid.NewServiceUserTokenId().String()
	valid, err := LoadDeploymentAdmissionIdentities(strings.NewReader(`{"acme":"` + id + `"}`))
	require.NoError(t, err)
	require.Equal(t, map[string]uuid.UUID{"acme": uuid.MustParse(id)}, valid)
	for _, input := range []string{`[]`, `null`, `{`, `{"acme":null}`, `{"acme":"` + id + `","acme":"` + id + `"}`, `{"":"` + id + `"}`, `{"acme":"00000000-0000-0000-0000-000000000000"}`, `{"acme":"ffffffff-ffff-ffff-ffff-ffffffffffff"}`, `{"acme":"` + strings.ToUpper(id) + `"}`, `{} {}`, `{"acme":"not-a-UUID"}`} {
		_, err := LoadDeploymentAdmissionIdentities(strings.NewReader(input))
		require.Error(t, err, input)
	}
	_, err = LoadDeploymentAdmissionIdentities(strings.NewReader(`{}` + strings.Repeat(" ", 1<<20)))
	require.ErrorContains(t, err, "bounded configuration")
	_, err = LoadDeploymentAdmissionIdentities(strings.NewReader(`{"acme":"` + userid.NewHumanUserId().String() + `"}`))
	require.ErrorContains(t, err, "service UUID")
}
