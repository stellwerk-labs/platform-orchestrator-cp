package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
