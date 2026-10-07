package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
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
	// journaled is false when no installed plugin subscribes to the
	// organization. The event is then not journaled, but the pause is recorded.
	journaled bool
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

// extensionJournalEntries projects the journal events among messages. Entries
// of organizations rejected by scope are marked not journaled: they keep only
// the ordinary outbox message.
func extensionJournalEntries(messages []*hstandardoutbox.PendingEventMessage, scope func(orgID string) bool) ([]extensionResourceJournalEntry, error) {
	entries := make([]extensionResourceJournalEntry, 0, len(messages))
	for _, message := range messages {
		entry, err := projectExtensionResourceEvent(message)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entry.journaled = scope(entry.orgID)
			entries = append(entries, *entry)
		}
	}
	return entries, nil
}

type journalOrganization struct {
	orgID     string
	journaled bool
	entries   []extensionResourceJournalEntry
}

// journalOrganizations groups a transaction's entries per organization in
// organization order, keeping event order within each organization. Every
// writer acquires journal state and head locks in this order.
func journalOrganizations(entries []extensionResourceJournalEntry) []journalOrganization {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b extensionResourceJournalEntry) int { return strings.Compare(a.orgID, b.orgID) })
	groups := []journalOrganization{}
	for _, entry := range sorted {
		if len(groups) == 0 || groups[len(groups)-1].orgID != entry.orgID {
			groups = append(groups, journalOrganization{orgID: entry.orgID, journaled: entry.journaled})
		}
		if entry.journaled {
			groups[len(groups)-1].entries = append(groups[len(groups)-1].entries, entry)
		}
	}
	return groups
}

func flushExtensionJournal(ctx context.Context, tx Tx, entries []extensionResourceJournalEntry) error {
	for _, org := range journalOrganizations(entries) {
		if !org.journaled {
			if err := pauseExtensionResourceJournal(ctx, tx, org.orgID); err != nil {
				return err
			}
			continue
		}
		if err := resumeExtensionResourceJournal(ctx, tx, org.orgID); err != nil {
			return err
		}
		for _, entry := range org.entries {
			if err := journalExtensionResourceEvent(ctx, tx, entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// pauseExtensionResourceJournal records that an organization with a journal
// is not journaled. A later resume then marks the gap.
func pauseExtensionResourceJournal(ctx context.Context, tx Tx, org string) error {
	var tracked bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT journaling FROM extension_resource_event_journal_state WHERE org_id=$1),
		EXISTS (SELECT 1 FROM extension_resource_event_heads WHERE org_id=$1))`, org).Scan(&tracked); err != nil || !tracked {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE extension_resource_event_journal_state SET journaling=false, changed_at=now() WHERE org_id=$1 AND journaling`, org); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO extension_resource_event_journal_state (org_id,journaling,changed_at)
		SELECT org_id,false,now() FROM extension_resource_event_heads WHERE org_id=$1 ON CONFLICT (org_id) DO NOTHING`, org)
	return err
}

// resumeExtensionResourceJournal marks a gap when the organization's journal
// was paused. The gap takes its own cursor: every cursor issued before it
// predates the unjournaled changes and expires; cursors from it on are valid.
func resumeExtensionResourceJournal(ctx context.Context, tx Tx, org string) error {
	paused := func(lock string) (bool, error) {
		var journaling bool
		err := tx.QueryRowContext(ctx, `SELECT journaling FROM extension_resource_event_journal_state WHERE org_id=$1`+lock, org).Scan(&journaling)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return !journaling, err
	}
	if stopped, err := paused(""); err != nil || !stopped {
		return err
	}
	if stopped, err := paused(" FOR UPDATE"); err != nil || !stopped {
		return err
	}
	var gap int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO extension_resource_event_heads (org_id,cursor) VALUES ($1,1)
		ON CONFLICT (org_id) DO UPDATE SET cursor=extension_resource_event_heads.cursor+1 RETURNING cursor`, org).Scan(&gap); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE extension_resource_event_journal_state SET journaling=true, gap_cursor=$2, changed_at=now() WHERE org_id=$1`, org, gap)
	return err
}

// ReconcileExtensionResourceJournal runs at startup, before the server takes
// requests. Each organization that has a journal is resumed when an installed
// plugin subscribes to it and paused otherwise. subscribed is nil when the
// plugin host is disabled.
func ReconcileExtensionResourceJournal(ctx context.Context, db Databaser, subscribed func(orgID string) bool) error {
	orgs, err := extensionJournalOrganizations(ctx, db, `SELECT org_id FROM extension_resource_event_heads
		UNION SELECT org_id FROM extension_resource_event_journal_state ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, org := range orgs {
		if err := reconcileExtensionResourceOrganization(ctx, db, org, subscribed != nil && subscribed(org)); err != nil {
			return err
		}
	}
	return nil
}

func reconcileExtensionResourceOrganization(ctx context.Context, db Databaser, org string, journaled bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if journaled {
		err = resumeExtensionResourceJournal(ctx, tx, org)
	} else {
		err = pauseExtensionResourceJournal(ctx, tx, org)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
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

// extensionResourceEventsRead is the delegated operation action that makes an
// operation a journal consumer. It matches the subscription service grant.
const extensionResourceEventsRead = "resource-events.read"

// ErrExtensionResourceCursorExpired reports that entries after the presented
// cursor were pruned or never journaled. Silently continuing would skip them.
var ErrExtensionResourceCursorExpired = errors.New("resource event cursor precedes retained history")

// ReadExtensionResourceEvents returns the next page within the grant's frozen
// scope. A nil cursor starts at the oldest retained entry. Presenting a cursor
// acknowledges every entry up to it: consumers present only cursors whose
// effects they committed, and retention never prunes past an active
// consumer's acknowledgement.
func ReadExtensionResourceEvents(ctx context.Context, db Databaser, grant ExternalOperationGrant, after *int64, limit int) ([]ExtensionResourceEvent, int64, error) {
	if (after != nil && *after < 0) || limit < 1 || limit > 200 {
		return nil, 0, NewErrBadRequest("invalid resource event page")
	}
	// Only subscribed Control Planes serve reads. A pause recorded by another
	// replica becomes a gap before anything is read.
	if err := resumeExtensionResourceJournalForRead(ctx, db, grant.OrgID); err != nil {
		return nil, 0, err
	}
	if after != nil {
		if err := acknowledgeExtensionResourceCursor(ctx, db, grant, *after); err != nil {
			return nil, 0, err
		}
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var head, horizon int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT cursor FROM extension_resource_event_heads WHERE org_id=$1),0),
		GREATEST(COALESCE((SELECT pruned_through FROM extension_resource_event_retention WHERE org_id=$1),0),
			COALESCE((SELECT gap_cursor FROM extension_resource_event_journal_state WHERE org_id=$1),0))`, grant.OrgID).Scan(&head, &horizon)
	if err != nil {
		return nil, 0, err
	}
	start, err := extensionResourceStart(after, head, horizon)
	if err != nil {
		return nil, 0, err
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
		AND (environment_uuid IS NOT NULL OR module_uuid IS NOT NULL) ORDER BY cursor LIMIT $6`, grant.OrgID, start, head, pq.Array(envs), pq.Array(modules), limit)
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

// extensionResourceStart returns the cursor a page starts after. History up
// to horizon, the later of the retention boundary and the latest journal gap,
// is unavailable: an older cursor expires and a read without a cursor starts
// at the horizon.
func extensionResourceStart(after *int64, head, horizon int64) (int64, error) {
	if after == nil {
		return horizon, nil
	}
	if *after > head {
		return 0, NewErrBadRequest("resource event cursor is ahead of this journal")
	}
	if *after < horizon {
		return 0, ErrExtensionResourceCursorExpired
	}
	return *after, nil
}

func resumeExtensionResourceJournalForRead(ctx context.Context, db Databaser, org string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := resumeExtensionResourceJournal(ctx, tx, org); err != nil {
		return err
	}
	return tx.Commit()
}

// acknowledgeExtensionResourceCursor moves the consumer's acknowledgement
// forward. A cursor beyond the journal head is not recorded; the read rejects it.
func acknowledgeExtensionResourceCursor(ctx context.Context, db Databaser, grant ExternalOperationGrant, cursor int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO extension_resource_event_consumers (org_id,plugin_id,operation_id,acknowledged_cursor)
		SELECT $1,$2,$3,$4 WHERE $4 <= COALESCE((SELECT cursor FROM extension_resource_event_heads WHERE org_id=$1),0)
		ON CONFLICT (org_id,plugin_id,operation_id) DO UPDATE SET acknowledged_cursor=EXCLUDED.acknowledged_cursor, acknowledged_at=now()
		WHERE extension_resource_event_consumers.acknowledged_cursor < EXCLUDED.acknowledged_cursor`,
		grant.OrgID, grant.PluginID, grant.OperationID, cursor); err != nil {
		return err
	}
	return tx.Commit()
}

// ExtensionResourceRetention is the explicit journal retention policy. An
// entry is pruned only when it is older than Window and below the lowest
// acknowledged cursor of every active consumer in its organization.
type ExtensionResourceRetention struct {
	Window time.Duration
	// Batch bounds the entries removed per organization and run.
	Batch int
}

func (p ExtensionResourceRetention) Validate() error {
	if p.Window < time.Hour {
		return errors.New("extension resource journal retention must be at least one hour")
	}
	if p.Batch < 1 || p.Batch > 100000 {
		return errors.New("extension resource journal retention batch must be between 1 and 100000")
	}
	return nil
}

// PruneExtensionResourceJournal applies the retention policy to every
// journaled organization. subscribers returns the plugins currently installed
// with a resource event subscription for an organization. Only their
// operations count as consumers: unreleased grants that delegate
// resource-events.read, and released ones for one more retention window so
// that terminal reconciliation can finish. A consumer without an
// acknowledgement holds its organization's entire history.
//
// Organizations are pruned under a try-advisory lock, so several Control
// Plane replicas may run this concurrently; an organization locked by another
// replica is skipped until the next run.
func PruneExtensionResourceJournal(ctx context.Context, db Databaser, subscribers func(orgID string) []string, policy ExtensionResourceRetention, now time.Time) (int64, error) {
	if err := policy.Validate(); err != nil {
		return 0, err
	}
	orgs, err := extensionJournalOrganizations(ctx, db, `SELECT org_id FROM extension_resource_event_heads ORDER BY org_id`)
	if err != nil {
		return 0, err
	}
	var pruned int64
	for _, org := range orgs {
		count, err := pruneExtensionResourceOrganization(ctx, db, org, subscribers(org), policy, now)
		if err != nil {
			return pruned, err
		}
		pruned += count
	}
	return pruned, nil
}

// extensionJournalReleaseCutoff is the release time after which a released
// operation still reconciles from Core: one journal retention window back.
func extensionJournalReleaseCutoff(now time.Time, window time.Duration) time.Time {
	return now.Add(-window)
}

// ExternalOperationReconciles reports whether an operation may still
// reconcile from Core without a user: prepared, active and suspended
// operations, matching the resource journal, and released ones for one journal
// retention window, during which the journal also keeps their history.
func ExternalOperationReconciles(ctx context.Context, db Databaser, grant ExternalOperationGrant, now time.Time, window time.Duration) (bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var updated time.Time
	if err := tx.QueryRowContext(ctx, `SELECT state, updated_at FROM external_operation_grants WHERE org_id=$1 AND plugin_id=$2 AND operation_id=$3`,
		grant.OrgID, grant.PluginID, grant.OperationID).Scan(&state, &updated); err != nil {
		return false, err
	}
	return externalOperationReconcilesAt(state, updated, now, window), nil
}

func externalOperationReconcilesAt(state string, updated, now time.Time, window time.Duration) bool {
	switch state {
	case grantStatePrepared, grantStateActive, grantStateSuspended:
		return true
	case grantStateReleased:
		return updated.After(extensionJournalReleaseCutoff(now, window))
	}
	return false
}

func extensionJournalOrganizations(ctx context.Context, db Databaser, query string) ([]string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	orgs := []string{}
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, rows.Err()
}

//	cursor:   1   2   3   4   5   6   7
//	          [ older than window ][young]
//	                    ^ lowest acknowledgement (5)
//	pruned:   1   2   3   4            -> pruned_through = 4
//
// The bound is the longest prefix after pruned_through whose entries are all
// older than the window and below the lowest acknowledgement, so retained
// history always stays contiguous.
func pruneExtensionResourceOrganization(ctx context.Context, db Databaser, org string, subscribers []string, policy ExtensionResourceRetention, now time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 4))`, "extension-resource-retention:"+org).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	}
	var prunedThrough int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT pruned_through FROM extension_resource_event_retention WHERE org_id=$1),0)`, org).Scan(&prunedThrough); err != nil {
		return 0, err
	}
	var lowest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(COALESCE(c.acknowledged_cursor,0)) FROM external_operation_grants g
		LEFT JOIN extension_resource_event_consumers c ON c.org_id=g.org_id AND c.plugin_id=g.plugin_id AND c.operation_id=g.operation_id
		WHERE g.org_id=$1 AND g.plugin_id=ANY($2) AND g.intent->'actions' @> jsonb_build_array($3::text)
		AND (g.state<>$4 OR g.updated_at>$5)`,
		org, pq.Array(subscribers), extensionResourceEventsRead, grantStateReleased, extensionJournalReleaseCutoff(now, policy.Window)).Scan(&lowest); err != nil {
		return 0, err
	}
	below := int64(math.MaxInt64)
	if lowest.Valid {
		below = lowest.Int64
	}
	var bound int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(cursor),$2) FROM (
		SELECT cursor, bool_and(recorded_at<$3 AND cursor<$4) OVER (ORDER BY cursor) AS prunable FROM (
			SELECT cursor, recorded_at FROM extension_resource_events WHERE org_id=$1 AND cursor>$2 ORDER BY cursor LIMIT $5
		) candidates
	) prefix WHERE prunable`, org, prunedThrough, now.Add(-policy.Window), below, policy.Batch).Scan(&bound); err != nil {
		return 0, err
	}
	if bound <= prunedThrough {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM extension_resource_events WHERE org_id=$1 AND cursor>$2 AND cursor<=$3`, org, prunedThrough, bound)
	if err != nil {
		return 0, err
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extension_resource_event_retention (org_id,pruned_through,pruned_at) VALUES ($1,$2,now())
		ON CONFLICT (org_id) DO UPDATE SET pruned_through=GREATEST(extension_resource_event_retention.pruned_through,EXCLUDED.pruned_through), pruned_at=now()`, org, bound); err != nil {
		return 0, err
	}
	return pruned, tx.Commit()
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
