package config

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hconfig"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigurationNATSFields(t *testing.T) {
	configuration := Configuration{
		NatsURL:              "nats://nats.example:4222",
		NatsToken:            "token",
		NatsCAFile:           "/certs/ca.crt",
		NatsStreamReplicas:   3,
		NatsBootstrapStreams: true,
	}
	assert.Equal(t, "nats://nats.example:4222", configuration.NatsURL)
	assert.Equal(t, "token", configuration.NatsToken)
	assert.Equal(t, "/certs/ca.crt", configuration.NatsCAFile)
	assert.Equal(t, 3, configuration.NatsStreamReplicas)
	assert.True(t, configuration.NatsBootstrapStreams)
}

// setRequiredEnvironment sets every variable Core requires to start.
func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASE_NAME": "orchestrator", "DATABASE_USER": "orchestrator", "DATABASE_PASSWORD": "unused",
		"DATA_PLANE_URL": "http://dp.example.test", "IAM_URL": "http://iam.example.test",
		"VAULT_URL": "http://vault.example.test", "VAULT_AUTH": "token",
	} {
		t.Setenv(key, value)
	}
}

// loadConfiguration loads the configuration from the environment as Core
// does at start-up.
func loadConfiguration() (Configuration, error) {
	var configuration Configuration
	err := hconfig.LoadConfigWithoutRetag(&configuration)
	return configuration, err
}

func TestPluginHostIsEnabledOnlyByItsReleasedVariable(t *testing.T) {
	setRequiredEnvironment(t)
	load := func(t *testing.T) Configuration {
		t.Helper()
		configuration, err := loadConfiguration()
		require.NoError(t, err)
		return configuration
	}
	defaults := load(t)
	assert.False(t, defaults.PluginHostEnabled, "the plugin host is off by default")
	assert.Equal(t, 720*time.Hour, defaults.PluginEventJournalRetention)

	t.Setenv("PLUGIN_EXPERIMENT_ENABLED", "true")
	assert.False(t, load(t).PluginHostEnabled, "the unreleased experiment variable has no effect")
	t.Setenv("PLUGIN_HOST_ENABLED", "true")
	assert.True(t, load(t).PluginHostEnabled)
}

func TestPluginManagerConfiguration(t *testing.T) {
	setRequiredEnvironment(t)
	defaults, err := loadConfiguration()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, defaults.PluginManagerRefreshInterval, "a replica serves changes within two seconds by default")

	t.Setenv("PLUGIN_MANAGER_REFRESH_INTERVAL", "500ms")
	configured, err := loadConfiguration()
	require.NoError(t, err)
	assert.Equal(t, 500*time.Millisecond, configured.PluginManagerRefreshInterval)

	for name, setting := range map[string][2]string{
		"refresh below 100ms":    {"PLUGIN_MANAGER_REFRESH_INTERVAL", "10ms"},
		"refresh above a minute": {"PLUGIN_MANAGER_REFRESH_INTERVAL", "2m"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(setting[0], setting[1])
			_, err := loadConfiguration()
			require.Error(t, err)
		})
	}
}

// The Data Plane holds one token for every organization, so Core names one
// identity for every organization and never a map of organizations: an
// organization created later is admitted without a configuration change.
func TestDeploymentAdmissionIsOneCanonicalServiceIdentity(t *testing.T) {
	setRequiredEnvironment(t)
	unset, err := loadConfiguration()
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, uuid.UUID(unset.DeploymentAdmissionIdentity), "unset, Core admits for nobody")
	t.Setenv("DEPLOYMENT_ADMISSION_IDENTITY", "")
	empty, err := loadConfiguration()
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, uuid.UUID(empty.DeploymentAdmissionIdentity), "an empty value is unset")

	service := userid.NewServiceUserTokenId()
	t.Setenv("DEPLOYMENT_ADMISSION_IDENTITY", service.String())
	configured, err := loadConfiguration()
	require.NoError(t, err)
	assert.Equal(t, service, uuid.UUID(configured.DeploymentAdmissionIdentity))

	for name, value := range map[string]string{
		"the nil UUID":                       uuid.Nil.String(),
		"Core's internal system identity":    userid.InternalSystemUuid.String(),
		"a human user":                       userid.NewHumanUserId().String(),
		"upper case":                         strings.ToUpper(service.String()),
		"without hyphens":                    strings.ReplaceAll(service.String(), "-", ""),
		"in braces":                          "{" + service.String() + "}",
		"as a URN":                           "urn:uuid:" + service.String(),
		"padded":                             " " + service.String(),
		"two identities":                     service.String() + "," + userid.NewServiceUserTokenId().String(),
		"an organization map":                `{"acme":"` + service.String() + `"}`,
		"the path of an identities document": "/var/run/secrets/stellwerk/deployment-admission/identities.json",
		"not a UUID":                         "data-plane",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DEPLOYMENT_ADMISSION_IDENTITY", value)
			_, err := loadConfiguration()
			require.ErrorContains(t, err, "must be the canonical UUID of an IAM service identity")
		})
	}
}
