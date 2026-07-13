package config

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
)

// Configuration ...
type Configuration struct {
	Port int `env:"PORT, default=8080" validate:"required"`

	DatabaseName     string `env:"DATABASE_NAME" validate:"required"`
	DatabaseUser     string `env:"DATABASE_USER" validate:"required"`
	DatabasePassword string `env:"DATABASE_PASSWORD" validate:"required"`
	DatabaseHost     string `env:"DATABASE_HOST"`
	DatabasePort     string `env:"DATABASE_PORT"`

	// AmqpConnectionString should be an AMQP url like "amqp://%s:%s@%s:%d/%s"
	AmqpConnectionString string `env:"AMQP_CONNECTION_STRING" validate:"omitempty,url"`

	// Alternatively, separate env vars can be set for AMQP connection
	AmpqHost     string `env:"AMQP_HOST"`
	AmpqPort     string `env:"AMQP_PORT, default=5672"`
	AmpqVhost    string `env:"AMQP_VHOST"`
	AmpqUsername string `env:"AMQP_USERNAME"`
	AmpqPassword string `env:"AMQP_PASSWORD"`

	// DataPlaneUrl is the api url for the data plane port 8080
	DataPlaneUrl string `env:"DATA_PLANE_URL" validate:"required,url"`

	// IamUrl is the internal url for the platform-orchestrator-iam service
	IamUrl string `env:"IAM_URL" validate:"required,url"`

	ShutdownDelay time.Duration `env:"SHUTDOWN_DELAY, default=10s"`
	OTELEnabled   bool          `env:"OTEL_ENABLE, default=false"`
	LogLevel      string        `env:"LOG_LEVEL, default=INFO"`

	VaultURL  string `env:"VAULT_URL" validate:"url"`
	VaultAuth string `env:"VAULT_AUTH" validate:"required"`
	VaultRole string `env:"VAULT_ROLE"`

}

func (c *Configuration) GetAmqpConnectionString() (string, error) {
	if c.AmqpConnectionString != "" {
		return c.AmqpConnectionString, nil
	}
	if c.AmpqHost == "" {
		return "", errors.New("AMQP_HOST or AMQP_CONNECTION_STRING is not set")
	}
	if c.AmpqVhost == "" {
		return "", errors.New("AMQP_VHOST or AMQP_CONNECTION_STRING is not set")
	}
	if c.AmpqUsername == "" {
		return "", errors.New("AMQP_USERNAME or AMQP_CONNECTION_STRING is not set")
	}
	if c.AmpqPassword == "" {
		return "", errors.New("AMQP_PASSWORD or AMQP_CONNECTION_STRING is not set")
	}
	return fmt.Sprintf("amqp://%s:%s@%s:%s/%s", c.AmpqUsername, c.AmpqPassword, c.AmpqHost, c.AmpqPort, c.AmpqVhost), nil
}
