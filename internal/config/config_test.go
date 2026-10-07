package config

import (
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hconfig"
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

func TestPluginHostIsEnabledOnlyByItsReleasedVariable(t *testing.T) {
	for key, value := range map[string]string{
		"DATABASE_NAME": "orchestrator", "DATABASE_USER": "orchestrator", "DATABASE_PASSWORD": "unused",
		"DATA_PLANE_URL": "http://dp.example.test", "IAM_URL": "http://iam.example.test",
		"VAULT_URL": "http://vault.example.test", "VAULT_AUTH": "token",
	} {
		t.Setenv(key, value)
	}
	load := func(t *testing.T) Configuration {
		t.Helper()
		configuration := Configuration{}
		require.NoError(t, hconfig.LoadConfigWithoutRetag(&configuration))
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
	for key, value := range map[string]string{
		"DATABASE_NAME": "orchestrator", "DATABASE_USER": "orchestrator", "DATABASE_PASSWORD": "unused",
		"DATA_PLANE_URL": "http://dp.example.test", "IAM_URL": "http://iam.example.test",
		"VAULT_URL": "http://vault.example.test", "VAULT_AUTH": "token",
	} {
		t.Setenv(key, value)
	}
	load := func() (Configuration, error) {
		configuration := Configuration{}
		return configuration, hconfig.LoadConfigWithoutRetag(&configuration)
	}
	defaults, err := load()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, defaults.PluginManagerRefreshInterval, "a replica serves changes within two seconds by default")

	t.Setenv("PLUGIN_MANAGER_REFRESH_INTERVAL", "500ms")
	configured, err := load()
	require.NoError(t, err)
	assert.Equal(t, 500*time.Millisecond, configured.PluginManagerRefreshInterval)

	for name, setting := range map[string][2]string{
		"refresh below 100ms":    {"PLUGIN_MANAGER_REFRESH_INTERVAL", "10ms"},
		"refresh above a minute": {"PLUGIN_MANAGER_REFRESH_INTERVAL", "2m"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(setting[0], setting[1])
			_, err := load()
			require.Error(t, err)
		})
	}
}
