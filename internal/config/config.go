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

	PluginExperimentEnabled bool   `env:"PLUGIN_EXPERIMENT_ENABLED, default=false"`
	PluginInstallationsFile string `env:"PLUGIN_INSTALLATIONS_FILE"`
	PluginSigningKeyFile    string `env:"PLUGIN_SIGNING_KEY_FILE"`
	PluginIssuer            string `env:"PLUGIN_ISSUER, default=stellwerk-control-plane"`
	PluginDevelopmentHTTP   bool   `env:"PLUGIN_DEVELOPMENT_HTTP, default=false"`
	PluginCAFile            string `env:"PLUGIN_CA_FILE"`
	// Entitlement trust is independent of package, transport and identity keys.
	// Production rejects development issuers even when signatures are valid.
	PluginEntitlementMode       string `env:"PLUGIN_ENTITLEMENT_MODE, default=production" validate:"oneof=production development"`
	PluginEntitlementTrustFile  string `env:"PLUGIN_ENTITLEMENT_TRUST_FILE"`
	PluginEntitlementBundleFile string `env:"PLUGIN_ENTITLEMENT_BUNDLE_FILE"`
	// PluginEventJournalRetention is the minimum age before an extension
	// resource journal entry that every active consumer acknowledged is pruned.
	PluginEventJournalRetention time.Duration `env:"PLUGIN_EVENT_JOURNAL_RETENTION, default=720h"`
	// PluginDataPlaneInternalHTTP permits only the fixed Core-to-DP origin in
	// an operator-declared trusted cluster network. It never permits HTTP
	// plugin backends or service-user bearer credentials over HTTP.
	PluginDataPlaneInternalHTTP       bool   `env:"PLUGIN_DATA_PLANE_INTERNAL_HTTP, default=false"`
	DeploymentAdmissionIdentitiesFile string `env:"DEPLOYMENT_ADMISSION_IDENTITIES_FILE"`

	VaultURL  string `env:"VAULT_URL" validate:"url"`
	VaultAuth string `env:"VAULT_AUTH" validate:"required"`
	VaultRole string `env:"VAULT_ROLE"`
}
