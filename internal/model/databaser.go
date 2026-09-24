package model

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"sync"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stellwerk-labs/golib/hmessaging/reliableoutbox"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/golib/hpostgresconnect"
	"github.com/stellwerk-labs/golib/hstandardoutbox"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
)

//go:generate go tool mockgen  -destination mocks/databaser.go github.com/stellwerk-labs/platform-orchestrator-cp/internal/model Databaser,TxWithCommit

//go:embed migrations/*.sql
var embedMigrations embed.FS

var registerGoMigrations sync.Once

const UniqueViolationErrorCode string = "unique_violation"

// Model is the underlying type for the entire model.
type databaser struct {
	*sql.DB
	logger *zap.Logger
}

type gooseZapLogger struct {
	*zap.SugaredLogger
}

func (g *gooseZapLogger) Printf(format string, v ...interface{}) {
	g.Infof(format, v...)
}

// NewDatabaser creates a new database provider instance
func NewDatabaser(ctx context.Context, logger *zap.Logger, connStr string) (Databaser, error) {
	goose.SetLogger(&gooseZapLogger{SugaredLogger: logger.Named("goose").Sugar()})
	goose.SetBaseFS(embedMigrations)
	goose.SetVerbose(logger.Level() <= zap.DebugLevel)
	registerGoMigrations.Do(func() {
		goose.AddNamedMigrationContext("000011_pending_event_messages.go", hstandardoutbox.MigrateUp01, hstandardoutbox.MigrateDown01)
	})

	if inner, err := hpostgresconnect.InitDatabase(ctx, &hpostgresconnect.Config{
		Logger:  logger,
		ConnStr: connStr,
	}); err != nil {
		return nil, err
	} else if err := goose.Up(inner.DB, "migrations"); err != nil {
		return nil, err
	} else {
		return &databaser{DB: inner.DB, logger: logger}, nil
	}
}

// Databaser provides an interface which can be used to mock the model
type Databaser interface {
	GetOrg(ctx context.Context, optionalTx Tx, id string) (*Org, error)
	ListOrgs(ctx context.Context, optionalTx Tx, pageToken string, perPage int, params ListOrgsParams) (items []Org, nextPageToken string, err error)
	CreateOrg(ctx context.Context, optionalTx Tx, request *Org) (*Org, error)
	UpdateOrg(ctx context.Context, optionalTx Tx, id string, update UpdateOrgRequest) (*Org, error)
	DeleteOrg(ctx context.Context, optionalTx Tx, id string) error

	ListEnvironmentTypes(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []EnvType, nextPageToken string, err error)
	CreateEnvironmentType(ctx context.Context, optionalTx Tx, request *EnvType) (*EnvType, error)
	GetEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string, mode GetMode) (*EnvType, error)
	DeleteEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string) error
	UpdateEnvironmentType(ctx context.Context, optionalTx Tx, orgId, id string, params UpdateEnvTypeParams) (*EnvType, error)

	ListProjects(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []Project, nextPageToken string, err error)
	CreateProject(ctx context.Context, optionalTx Tx, request *Project) (*Project, error)
	GetProject(ctx context.Context, optionalTx Tx, orgId, id string, mode GetMode) (*Project, error)
	GetProjectByUuid(ctx context.Context, optionalTx Tx, orgId string, uuid uuid.UUID, mode GetMode) (*Project, error)
	UpdatedProject(ctx context.Context, optional Tx, orgId, id string, params UpdateProjectParams) (*Project, error)
	DeleteProject(ctx context.Context, optionalTx Tx, orgId, id string) error

	ListEnvironments(ctx context.Context, optionalTx Tx, orgId, projectId string, pageToken string, perPage int, params ListEnvironmentsParams) (items []Environment, nextPageToken string, err error)
	ListEnvironmentsInOrg(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListEnvironmentsParams) (items []Environment, nextPageToken string, err error)
	ListEnvironmentsByProjectUuid(ctx context.Context, optionalTx Tx, orgId string, projectUuid uuid.UUID, pageToken string, perPage int) (items []Environment, nextPageToken string, err error)
	ListEnvironmentsByRunnerId(ctx context.Context, optionalTx Tx, orgId, runnerId string, pageToken string, perPage int) (items []Environment, nextPageToken string, err error)
	CreateEnvironment(ctx context.Context, optionalTx Tx, request *Environment) (*Environment, error)
	GetEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string, mode GetMode) (*Environment, error)
	GetEnvironmentByUuid(ctx context.Context, optionalTx Tx, orgId string, uuid uuid.UUID, mode GetMode) (*Environment, error)
	DeleteEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string) error
	UpdateEnvironment(ctx context.Context, optionalTx Tx, orgId, projectId, id string, request *EnvironmentPatch) (*Environment, error)

	ListResourceTypes(ctx context.Context, optionalTx Tx, orgId *string, pageToken string, perPage int) (items []ResourceType, nextPageToken string, err error)
	BulkGetResourceTypes(ctx context.Context, optionalTx Tx, orgId string, ids []string) (items []ResourceType, err error)
	GetResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string) (*ResourceType, error)
	CreateResourceType(ctx context.Context, optionalTx Tx, request *ResourceType) (*ResourceType, error)
	UpdateResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string, request *ResourceTypePatch) (*ResourceType, error)
	SetResourceTypeCatalogueStatus(ctx context.Context, tx Tx, orgID, id, target string, actor uuid.UUID, reason string, expectedVersion int64) (*ResourceType, error)
	DeleteResourceType(ctx context.Context, optionalTx Tx, orgId *string, id string) error

	ListAvailableResourceTypes(ctx context.Context, optionalTx Tx, orgId, projectId, envId string, pageToken string, perPage int, filters ListAvailableResourceTypeParams) ([]AvailableResourceType, string, error)

	ListModuleProviders(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleProvidersParams) (items []ModuleProvider, nextPageToken string, err error)
	CreateModuleProvider(ctx context.Context, optionalTx Tx, request *ModuleProvider) (*ModuleProvider, error)
	UpdateModuleProvider(ctx context.Context, optionalTx Tx, request *ModuleProvider) (*ModuleProvider, error)
	GetModuleProvider(ctx context.Context, optionalTx Tx, orgId, providerType, id string) (*ModuleProvider, error)
	DeleteModuleProvider(ctx context.Context, optionalTx Tx, orgId, providerType, id string) error

	ListModuleDefinitions(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleDefinitionsParams) (items []ModuleDefinitionVersion, nextPageToken string, err error)
	ListModuleDefinitionVersions(ctx context.Context, optionalTx Tx, orgId string, defId string, pageToken string, perPage int, params ListModuleDefinitionVersionsParams) (items []ModuleDefinitionVersion, nextPageToken string, err error)
	GetModuleDefinition(ctx context.Context, optionalTx Tx, orgId, defId string, mode GetMode) (*ModuleDefinitionVersion, error)
	GetModuleDefinitionVersion(ctx context.Context, optionalTx Tx, orgId, defId string, versionId string) (*ModuleDefinitionVersion, error)
	BulkGetModuleDefinitionVersions(ctx context.Context, optionalTx Tx, orgId string, defIds []string, versionIds []string) ([]ModuleDefinitionVersion, error)
	CreateModuleDefinition(ctx context.Context, optionalTx Tx, request *ModuleDefinitionVersion) (*ModuleDefinitionVersion, error)
	CreateModuleDefinitionVersion(ctx context.Context, tx Tx, request *ModuleDefinitionVersion) (*ModuleDefinitionVersion, error)
	DeleteModuleDefinitionVersion(ctx context.Context, optionalTx Tx, orgId, defId string, versionId string) error
	DeleteModuleDefinition(ctx context.Context, optionalTx Tx, orgId, defId string) error
	ListModuleCatalogues(ctx context.Context, optionalTx Tx, orgID string, includeArchived bool) ([]ModuleCatalogue, error)
	GetModuleCatalogue(ctx context.Context, optionalTx Tx, orgID, moduleRef string, mode GetMode) (*ModuleCatalogue, error)
	CreateEmptyModule(ctx context.Context, tx Tx, orgID, slug, displayName, description, resourceType string, tags map[string]string) (*ModuleCatalogue, error)
	UpdateModuleCatalogueMetadata(ctx context.Context, tx Tx, orgID, moduleRef, displayName, description string, tags map[string]string, expectedVersion int64) (*ModuleCatalogue, error)
	SetModuleCatalogueStatus(ctx context.Context, tx Tx, orgID, moduleRef string, target moduleversions.CatalogueStatus, actor uuid.UUID, reason string, expectedVersion int64) (*ModuleCatalogue, error)
	GetCoreModuleVersion(ctx context.Context, optionalTx Tx, orgID, moduleRef, versionRef string, mode GetMode) (*CoreModuleVersion, error)
	ListCoreModuleVersions(ctx context.Context, optionalTx Tx, orgID, moduleRef string, includeDeprecated, includeDefective bool) ([]CoreModuleVersion, error)
	TransitionCoreModuleVersion(ctx context.Context, tx Tx, orgID, moduleRef, versionRef string, target moduleversions.LifecycleStatus, expectedVersion int64, actor uuid.UUID, reason string, correlationID *uuid.UUID) (*CoreModuleVersion, error)
	ListModuleLifecycleEvents(ctx context.Context, optionalTx Tx, orgID, moduleRef, versionRef string) ([]ModuleLifecycleEvent, error)
	PublishCoreModuleVersion(ctx context.Context, tx Tx, request *ModuleDefinitionVersion, actor uuid.UUID) (*CoreModuleVersion, error)
	PublishStableModuleVersionSuccessor(ctx context.Context, tx Tx, orgID, moduleRef, prereleaseRef string, expectedPrereleaseVersion int64, reason string, request *ModuleDefinitionVersion, actor uuid.UUID) (*StableModuleVersionSuccessor, error)
	GetEnvironmentModuleVersionPin(ctx context.Context, optionalTx Tx, orgID string, pinID uuid.UUID, mode GetMode) (*EnvironmentModuleVersionPin, error)
	ListEnvironmentModuleVersionPins(ctx context.Context, optionalTx Tx, orgID string, environmentUUID *uuid.UUID, moduleUUID *uuid.UUID, includeRemoved bool) ([]EnvironmentModuleVersionPin, error)
	CreateEnvironmentModuleVersionPin(ctx context.Context, tx Tx, orgID string, projectUUID uuid.UUID, projectID string, environmentUUID uuid.UUID, environmentID string, moduleUUID, versionUUID, actor uuid.UUID, actorType, reason string, bulkOperationID *uuid.UUID, allowDefective bool) (*EnvironmentModuleVersionPin, error)
	TransitionEnvironmentModuleVersionPin(ctx context.Context, tx Tx, orgID string, pinID uuid.UUID, expectedVersion int64, target moduleversions.PinStatus, eventType string, actor uuid.UUID, actorType, reason string, operationID, targetVersionUUID, deploymentID *uuid.UUID) (*EnvironmentModuleVersionPin, error)
	GetModuleOperationReservation(ctx context.Context, optionalTx Tx, orgID string, reservationID uuid.UUID, mode GetMode) (*ModuleOperationReservation, error)
	ListModuleOperationReservations(ctx context.Context, optionalTx Tx, orgID string, moduleUUID uuid.UUID, includeReleased bool) ([]ModuleOperationReservation, error)
	AcquireModuleOperationReservation(ctx context.Context, tx Tx, orgID, moduleRef, namespace string, operationID uuid.UUID, relatedResourceID, reason string, actor uuid.UUID) (*ModuleOperationReservation, error)
	ReleaseModuleOperationReservation(ctx context.Context, tx Tx, orgID string, reservationID uuid.UUID, expectedVersion int64, actor uuid.UUID, reason string) (*ModuleOperationReservation, error)
	UpsertModuleExtensionContribution(ctx context.Context, tx Tx, orgID, moduleRef string, versionUUID, environmentUUID *uuid.UUID, namespace, externalResourceID, kind, lifecycleState, label, targetURL string, payload map[string]any, actor uuid.UUID) (*ModuleExtensionContribution, error)
	ListModuleExtensionContributions(ctx context.Context, optionalTx Tx, orgID string, moduleUUID uuid.UUID, includeDraft, includeTerminal bool) ([]ModuleExtensionContribution, error)
	ListModuleExtensionContributionsForEnvironment(ctx context.Context, optionalTx Tx, orgID string, environmentUUID uuid.UUID, includeDraft, includeTerminal bool) ([]ModuleExtensionContribution, error)
	ListModuleVersionPinEvents(ctx context.Context, optionalTx Tx, orgID string, pinID uuid.UUID) ([]ModuleVersionPinEvent, error)
	AppendModuleVersionPinNote(ctx context.Context, tx Tx, orgID string, pinID uuid.UUID, actor uuid.UUID, actorType, note string) (*ModuleVersionPinEvent, error)
	RemoveEnvironmentModuleVersionPinsForDeletion(ctx context.Context, tx Tx, orgID string, environmentUUID, actor uuid.UUID, actorType string) ([]EnvironmentModuleVersionPin, error)
	GetModuleCoreCommand(ctx context.Context, optionalTx Tx, orgID, scope, key string) (string, json.RawMessage, bool, error)
	StoreModuleCoreCommand(ctx context.Context, tx Tx, orgID, scope, key, fingerprint string, actor uuid.UUID, response any) error

	ListModuleRules(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListModuleRulesParams) (items []DefinitionRule, nextPageToken string, err error)
	CreateModuleRule(ctx context.Context, optionalTx Tx, orgId string, request *DefinitionRule) (*DefinitionRule, error)
	GetModuleRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) (*DefinitionRule, error)
	DeleteModuleRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) error
	BulkDeleteModuleRuleDefinitions(ctx context.Context, tx Tx, orgId string, params DeleteModuleRulesParams) ([]string, error)

	ListRunners(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int) (items []Runner, nextPageToken string, err error)
	GetRunner(ctx context.Context, optionalTx Tx, orgId, id string) (*Runner, error)
	CreateRunner(ctx context.Context, tx Tx, request *Runner) (*Runner, error)
	DeleteRunner(ctx context.Context, optionalTx Tx, orgId, id string) error
	UpdateRunner(ctx context.Context, optionalTx Tx, orgId, id string, request *RunnerPatch) (*Runner, error)

	ListRunnerRules(ctx context.Context, optionalTx Tx, orgId string, pageToken string, perPage int, params ListRunnerRulesParams) (items []RunnerRule, nextPageToken string, err error)
	CreateRunnerRule(ctx context.Context, tx Tx, orgId string, request *RunnerRule) (*RunnerRule, error)
	GetRunnerRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) (*RunnerRule, error)
	DeleteRunnerRule(ctx context.Context, optionalTx Tx, orgId, ruleId string) error
	BulkDeleteRunnerRules(ctx context.Context, tx Tx, orgId string, params DeleteRunnerRulesParams) ([]string, error)

	Close() error
	BeginTx(ctx context.Context, opts *sql.TxOptions) (TxWithCommit, error)

	AsReliableOutboxStore() reliableoutbox.Store[*hstandardoutbox.PendingEventMessage]
	InsertPendingEventMessages(ctx context.Context, optionalTx Tx, messages []*hstandardoutbox.PendingEventMessage) ([]*hstandardoutbox.PendingEventMessage, error)
}

type Tx interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

type TxWithCommit interface {
	Tx
	Commit() error
	Rollback() error
}

func (d *databaser) txOrDb(optionalTx Tx) Tx {
	if optionalTx == nil {
		return d
	}
	return optionalTx
}

func (d *databaser) BeginTx(ctx context.Context, opts *sql.TxOptions) (TxWithCommit, error) {
	return d.DB.BeginTx(ctx, opts)
}

func (d *databaser) AsReliableOutboxStore() reliableoutbox.Store[*hstandardoutbox.PendingEventMessage] {
	return hstandardoutbox.SQLContextAsReliableOutbox(d.DB)
}

func (d *databaser) InsertPendingEventMessages(ctx context.Context, optionalTx Tx, messages []*hstandardoutbox.PendingEventMessage) ([]*hstandardoutbox.PendingEventMessage, error) {
	return hstandardoutbox.InsertPendingEventMessages(ctx, d.txOrDb(optionalTx), messages)
}

type GetMode string

const (
	GetModeDefault   GetMode = "default"
	GetModeForUpdate GetMode = "for_update"
)

func GetModeSuffix(mode GetMode) string {
	switch mode {
	case GetModeForUpdate:
		return " FOR UPDATE"
	default:
		return ""
	}
}
