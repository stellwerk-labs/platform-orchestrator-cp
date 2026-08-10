package config

import "time"

// Configuration ...
type Configuration struct {
	Port int `env:"PORT, default=8080" validate:"required"`

	DatabaseName     string `env:"DATABASE_NAME" validate:"required"`
	DatabaseUser     string `env:"DATABASE_USER" validate:"required"`
	DatabasePassword string `env:"DATABASE_PASSWORD" validate:"required"`
	DatabaseHost     string `env:"DATABASE_HOST"`
	DatabasePort     string `env:"DATABASE_PORT"`

	NatsURL              string `env:"NATS_URL, default=nats://localhost:4222" validate:"required,url"`
	NatsToken            string `env:"NATS_TOKEN"`
	NatsCredentialsFile  string `env:"NATS_CREDENTIALS_FILE"`
	NatsNKeySeedFile     string `env:"NATS_NKEY_SEED_FILE"`
	NatsCAFile           string `env:"NATS_CA_FILE"`
	NatsClientCertFile   string `env:"NATS_CLIENT_CERT_FILE"`
	NatsClientKeyFile    string `env:"NATS_CLIENT_KEY_FILE"`
	NatsTLSServerName    string `env:"NATS_TLS_SERVER_NAME"`
	NatsStreamReplicas   int    `env:"NATS_STREAM_REPLICAS, default=1" validate:"min=1,max=5"`
	NatsBootstrapStreams bool   `env:"NATS_BOOTSTRAP_STREAMS, default=false"`

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
