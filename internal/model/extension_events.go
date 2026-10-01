package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/golib/hstandardoutbox"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genevents"
)

// ExtensionResourceEvent is a deliberately narrow metadata projection. Module
// definitions, Pin notes, credentials and deployment material never enter it.
type ExtensionResourceEvent struct {
	EventID           uuid.UUID  `json:"event_id"`
	Type              string     `json:"type"`
	OccurredAt        time.Time  `json:"occurred_at"`
	EnvironmentUUID   *uuid.UUID `json:"environment_uuid,omitempty"`
	ProjectUUID       *uuid.UUID `json:"project_uuid,omitempty"`
	ModuleUUID        *uuid.UUID `json:"module_uuid,omitempty"`
	VersionUUID       *uuid.UUID `json:"version_uuid,omitempty"`
	PinID             *uuid.UUID `json:"pin_id,omitempty"`
	ActivationEventID *uuid.UUID `json:"activation_event_id,omitempty"`
	OperationID       *uuid.UUID `json:"operation_id,omitempty"`
	ActorID           *uuid.UUID `json:"actor_id,omitempty"`
	FromStatus        string     `json:"from_status,omitempty"`
	ToStatus          string     `json:"to_status,omitempty"`
	Transition        string     `json:"transition,omitempty"`
	Reason            string     `json:"reason,omitempty"`
}

type extensionResourceJournalEntry struct {
	orgID string
	event ExtensionResourceEvent
}

func projectExtensionResourceEvent(message *hstandardoutbox.PendingEventMessage) (*extensionResourceJournalEntry, error) {
	switch genevents.EventType(message.Subject) {
	case genevents.IoPlatformOrchestratorEnvironmentDeleted, genevents.IoPlatformOrchestratorModuleCatalogueChanged,
		genevents.IoPlatformOrchestratorModuleVersionPublished, genevents.IoPlatformOrchestratorModuleVersionLifecycleChanged,
		genevents.IoPlatformOrchestratorModuleVersionPinLifecycleChanged:
	default:
		return nil, nil
	}
	var envelope struct {
		Time time.Time `json:"time"`
		Data struct {
			OrgID           string     `json:"org_id"`
			EnvUUID         *uuid.UUID `json:"env_uuid"`
			Actor           *uuid.UUID `json:"actor"`
			EventType       string     `json:"event_type"`
			CatalogueStatus string     `json:"catalogue_status"`
			LifecycleStatus string     `json:"lifecycle_status"`
			ExtensionResourceEvent
		} `json:"data"`
	}
	if err := json.Unmarshal(message.Payload, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data.OrgID == "" {
		return nil, NewErrBadRequest("resource event requires an organization")
	}
	event := envelope.Data.ExtensionResourceEvent
	event.EventID, event.Type, event.OccurredAt = uuid.New(), message.Subject, envelope.Time
	event.ActorID, event.Transition = envelope.Data.Actor, envelope.Data.EventType
	if message.Subject == string(genevents.IoPlatformOrchestratorModuleCatalogueChanged) {
		event.ToStatus = envelope.Data.CatalogueStatus
	}
	if message.Subject == string(genevents.IoPlatformOrchestratorModuleVersionPublished) {
		event.ToStatus = envelope.Data.LifecycleStatus
	}
	if event.EnvironmentUUID == nil {
		event.EnvironmentUUID = envelope.Data.EnvUUID
	}
	return &extensionResourceJournalEntry{orgID: envelope.Data.OrgID, event: event}, nil
}

func journalExtensionResourceEvent(ctx context.Context, tx Tx, entry extensionResourceJournalEntry) error {
	encoded, err := json.Marshal(entry.event)
	if err != nil {
		return err
	}
	// The row lock serializes cursor allocation through commit. A sequence alone
	// would let a reader skip an earlier, still-uncommitted transaction forever.
	var cursor int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO extension_resource_event_heads (org_id,cursor) VALUES ($1,1)
		ON CONFLICT (org_id) DO UPDATE SET cursor=extension_resource_event_heads.cursor+1 RETURNING cursor`, entry.orgID).Scan(&cursor); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO extension_resource_events (org_id,cursor,event_id,environment_uuid,module_uuid,event)
		VALUES ($1,$2,$3,$4,$5,$6)`, entry.orgID, cursor, entry.event.EventID, entry.event.EnvironmentUUID, entry.event.ModuleUUID, string(encoded))
	return err
}

func ReadExtensionResourceEvents(ctx context.Context, db Databaser, grant ExternalOperationGrant, after int64, limit int) ([]ExtensionResourceEvent, int64, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return nil, 0, NewErrBadRequest("invalid resource event page")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var head int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT cursor FROM extension_resource_event_heads WHERE org_id=$1),0)`, grant.OrgID).Scan(&head)
	if err != nil {
		return nil, 0, err
	}
	if after > head {
		return nil, 0, NewErrBadRequest("resource event cursor is ahead of this journal")
	}
	envs := make([]string, 0, len(grant.Intent.Environments))
	modules := make([]string, 0, len(grant.Intent.Modules))
	for _, env := range grant.Intent.Environments {
		envs = append(envs, env.EnvironmentID.String())
	}
	for _, module := range grant.Intent.Modules {
		modules = append(modules, module.String())
	}
	rows, err := tx.QueryContext(ctx, `SELECT cursor,event FROM extension_resource_events WHERE org_id=$1 AND cursor>$2 AND cursor<=$3
		AND (environment_uuid IS NULL OR environment_uuid=ANY($4::uuid[]))
		AND (module_uuid IS NULL OR module_uuid=ANY($5::uuid[]))
		AND (environment_uuid IS NOT NULL OR module_uuid IS NOT NULL) ORDER BY cursor LIMIT $6`, grant.OrgID, after, head, pq.Array(envs), pq.Array(modules), limit)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	items := []ExtensionResourceEvent{}
	next := head
	for rows.Next() {
		var cursor int64
		var data []byte
		var item ExtensionResourceEvent
		if err := rows.Scan(&cursor, &data); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal(data, &item); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
		if len(items) == limit {
			next = cursor
		}
	}
	return items, next, rows.Err()
}

type ExtensionPublishedEvent struct {
	EventID    uuid.UUID      `json:"event_id"`
	GrantFence int64          `json:"grant_fence"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Data       map[string]any `json:"data"`
}

var (
	ErrExtensionEventReused = errors.New("event ID already identifies another publication")
	ErrExtensionEventFence  = errors.New("event publication grant fence changed")
	extensionEventSuffix    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
)

func validExtensionEventType(pluginID, eventType string) bool {
	prefix := "plugin." + pluginID + "."
	return len(eventType) <= 200 && strings.HasPrefix(eventType, prefix) && extensionEventSuffix.MatchString(strings.TrimPrefix(eventType, prefix))
}

// PublishExtensionEvent atomically retains the exact request receipt and writes
// the existing transactional outbox. Consumers deduplicate by CloudEvent id.
func PublishExtensionEvent(ctx context.Context, db Databaser, grant ExternalOperationGrant, event ExtensionPublishedEvent) error {
	if event.EventID == uuid.Nil || event.OccurredAt.IsZero() || !validExtensionEventType(grant.PluginID, event.Type) {
		return NewErrBadRequest("an immutable event ID, time and owned event type with one local name segment are required")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(encoded) > 65536 {
		return NewErrBadRequest("plugin event exceeds 64 KiB")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Grant locking serializes event receipts and control commands without
	// reactivating authority. Terminal events may follow grant release.
	var fence int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM external_operation_grants WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3 FOR UPDATE`, grant.OrgID, grant.PluginID, grant.OperationID).Scan(&fence); err != nil {
		return err
	}
	var matches bool
	err = tx.QueryRowContext(ctx, `SELECT request=$4::jsonb AND operation_id=$3 FROM extension_published_events WHERE org_id=$1 AND plugin_id=$2 AND event_id=$5`, grant.OrgID, grant.PluginID, grant.OperationID, string(encoded), event.EventID).Scan(&matches)
	if err == nil {
		if !matches {
			return ErrExtensionEventReused
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if fence != event.GrantFence {
		return ErrExtensionEventFence
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extension_published_events(org_id,plugin_id,operation_id,event_id,request) VALUES ($1,$2,$3,$4,$5)`, grant.OrgID, grant.PluginID, grant.OperationID, event.EventID, string(encoded)); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"specversion": "1.0", "id": event.EventID, "source": "plugin:" + grant.PluginID, "type": event.Type, "time": event.OccurredAt, "org_id": grant.OrgID, "operation_id": grant.OperationID, "data": event.Data})
	if err != nil {
		return err
	}
	if _, err := db.InsertPendingEventMessages(ctx, tx, []*hstandardoutbox.PendingEventMessage{{Subject: event.Type, Payload: payload}}); err != nil {
		return err
	}
	return tx.Commit()
}
