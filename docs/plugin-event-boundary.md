# Plugin event boundary

Core keeps a durable metadata journal alongside its transactional event outbox.
Both writes share the transaction that changes the resource. Plugins read this
journal through `GET /orgs/{org}/plugins/{plugin}/operations/{operation}/resource-events`.
An Environment's authoritative deletion event is the signal to reconcile its
removal. A failed lookup, timeout or 404 is not that signal.

## Scope and recovery

Only the configured plugin service identity can read. The frozen operation must
delegate `resource-events.read`, the operator must register that service grant,
and its controller must still pass current scoped IAM checks. The returned
events are restricted to the operation's immutable Environment and Module UUIDs.
Suspended and released operations can reconcile; disabled or unentitled plugins
retain this safety path. Incompatible and unavailable installations fail closed.

The journal contains Environment deletion, Module catalogue and Version lifecycle,
publication, and Pin lifecycle metadata. It excludes Module definitions, Pin note
text, deployment logs, outputs and credentials. A Pin `note_added` transition is
context only and must not invalidate approvals or change its activation boundary.

Cursors are opaque, scoped to an organization/plugin/operation, and commit-ordered.
Core buffers the metadata projection inside its transaction and flushes it just
before commit, after resource locks have been acquired. Organization heads are
locked in stable order. This keeps cursor allocation commit-ordered without
introducing a head-lock/resource-lock inversion in bulk commands. A database
sequence alone could skip a lower cursor that commits later. A page
may contain no matching events while still advancing its cursor. Consumers must
commit their cursor and effects atomically, and deduplicate by immutable event ID.
Migration 37 refuses a schema downgrade that would discard event history.
Application rollback keeps the additive tables.

## Journal scope

Core keeps the journal only for organizations in which an installed plugin
subscribes: its installation lists the organization and has a
`resource-events.read` service grant. The installation registry is the only
source; there is no separate switch. A Control Plane without the plugin host
(`PLUGIN_HOST_ENABLED=false`), or without any subscribing installation,
writes no journal entries and uses the plain transactional outbox path. Ordinary
outbox events are unaffected either way.

A consumer's history starts when its subscription is installed. Core does not
synthesize entries for changes made before that, or while no plugin subscribed.
Initial planning reads authoritative current resource inventory, not history.
The registry is read at startup, so installing or removing a subscription takes
effect with the restart that applies the new installation configuration.

When journaling of an organization pauses and later resumes, Core records a
gap instead of letting a returning subscriber silently miss changes. A pause is
recorded at startup by every Control Plane that does not journal the
organization (subscription removed or plugin host disabled), and by a plugin
host that skips an event of an organization with a journal. Journaling resumes
at the startup of a subscribed Control Plane, or at its next journal write or
read. The resume takes a gap cursor of its own. Every cursor issued before the
latest gap returns HTTP 409 `plugin_resource_cursor_expired`, exactly like a
cursor before pruned history, so the consumer resynchronises from current Core
resources and reads again without a cursor. Reading without a cursor starts
after the gap. A Control Plane with the plugin host disabled records pauses
only at its startup, so turn the plugin host on or off for all replicas
together, not mixed.

## Acknowledgement and retention

Presenting a cursor as `after` acknowledges every entry up to it. Present only a
cursor whose effects you have committed; never read ahead with an uncommitted
cursor. Reading without a cursor starts at the oldest retained entry.

The retention policy removes an entry only when both hold:

- it is older than `PLUGIN_EVENT_JOURNAL_RETENTION` (default `720h`, 30 days;
  a value below one hour stops startup), and
- its cursor is below the lowest acknowledgement of every active consumer in
  the organization.

Active consumers are operations of currently installed subscribing plugins whose
frozen intent delegates `resource-events.read` and that are not released, plus
released operations for one further retention window so terminal reconciliation
can finish. An active consumer that never presented a cursor holds its
organization's entire history, so release abandoned operations. Operations of an
uninstalled plugin do not hold history.

Pruning always removes a contiguous prefix, so retained history has no holes. A
cursor older than the pruned history returns HTTP 409
`plugin_resource_cursor_expired` instead of silently skipping entries. The
consumer then rebuilds its state from current Core resources (see
Resynchronisation) and reads again without a cursor. Retention never prunes
past an active consumer's cursor, so this only affects consumers that were
inactive or restored an older cursor.

Every Control Plane replica runs retention at startup and then every ten
minutes, removing at most 10,000 entries per organization and run. Each
organization is pruned under a PostgreSQL try-advisory lock, so a concurrent
replica skips it and no replica waits for another. Retention runs only with the
plugin host enabled; without it the journal neither grows nor shrinks. Request
handling never depends on a retention run.

Retention is an explicit runtime policy, not a schema change. No down migration
deletes delivered history: migration 37 still refuses to discard journal or
publication history, and migration 39 refuses to downgrade once any entry was
pruned, because the previous application could not report expired cursors.
Migration 40, which records journal pauses and gaps, likewise refuses to
downgrade once a pause or gap was recorded.
Rolling back only the application after pruning keeps the tables but loses that
expiry signal for stale cursors. Active consumers are unaffected, because their
history was never pruned. Entries that existed before migration 39 count their
age from the upgrade.

## Resynchronisation

After `plugin_resource_cursor_expired` a consumer rebuilds its state from
current Core resources without a user:

```text
GET /orgs/{org}/plugins/{plugin}/operations/{operation}/inventory
```

The response is the `PluginInventory` snapshot of the user-delegated
`POST /orgs/{org}/plugins/{plugin}/inventory`, but for exactly the operation's
frozen Module and Environment UUIDs, the scope its resource journal exposes. It
accepts no selector, so it can never widen that scope; an operation without
Environments sees none. Only the installation's configured service identity may
call. As with journal safety reconciliation, active and suspended operations may
read, and released ones for one `PLUGIN_EVENT_JOURNAL_RETENTION` window after
their release. Prepared operations return 409 `plugin_operation_conflict`, as do
operations released longer ago; unknown operations return 404
`plugin_operation_not_found`. The operation's controller must still pass current
scoped IAM, as for the journal. The user-delegated inventory read is unchanged.

The plugin declares the read exactly like the journal subscription:

- an installation `service_grants` entry with `"action": "inventory.read"`, the
  plugin's own `permission` and a `scope` of `environment` or `organization`;
- `inventory.read` in the `service_actions` of the plugin operation that
  prepares the operation grant;
- `inventory.read` in the frozen intent `actions` of that grant.

Without any of these the read returns 403 `plugin_service_forbidden`. Grants
prepared before the plugin declared `inventory.read` cannot read unattended.

## Publication

`POST /orgs/{org}/plugins/{plugin}/operations/{operation}/events` requires
`events.publish` in both the frozen operation and operator service policy. Core
rechecks current controller IAM and the exact grant fence. The immutable
`event_id` deduplicates retries, including a lost acknowledgement. Reusing it for
a different payload or operation returns a conflict. A receipt replay may follow
a fence rotation but cannot publish a new event under the stale fence.

The two 409 responses are distinct. `plugin_event_id_conflict` means a different
receipt already exists and must not be overwritten. `plugin_event_stale_fence`
means no receipt exists for that event ID under the grant lock; the publisher
may read the current grant and retry with that fence. A delayed old-fence request
cannot create a publication after this check. These stable codes avoid guessing
whether a lost acknowledgement committed.

Types must be `plugin.<immutable-plugin-id>.<local-name>`, where the local name
is one non-dotted segment (for example `rollout-completed`). Otherwise a parent
plugin ID could imitate a nested plugin's event namespace. A plugin cannot
publish as Core or another plugin. Payloads are bounded to 64 KiB and must not contain secret
values or deployment material. The event receipt and existing Core outbox are
written atomically. Delivery is at least once, not exactly once. Subscribers use
the CloudEvent `id` to deduplicate. No separate broker or execution path is added.

The CloudEvent type and broker subject are distinct. At delivery, Core maps
`plugin.<immutable-plugin-id>.<event>` to the transport subject
`io.platform-orchestrator.plugin.<immutable-plugin-id>.<event>`. The existing
`PO_EVENTS` stream already retains `io.platform-orchestrator.>`; both application
and Helm bootstrap keep that topology unchanged. Mapping at the publisher also
recovers already-persisted pending plugin messages. It changes neither payload,
CloudEvent type, outbox deduplication ID, trace headers nor expiry. Core event
subjects pass through unchanged.

This follows [NATS subject-based stream retention](https://docs.nats.io/learn/jetstream/your-first-stream).
The explicit integration gate uses a dedicated empty loopback JetStream:

```sh
PLUGIN_EVENTS_TEST_NATS_URL=nats://127.0.0.1:<disposable-port> \
PLUGIN_EVENTS_TEST_DISPOSABLE=1 \
go test -race -tags integration ./internal/events \
  -run TestPluginPublicationUsesExistingJetStreamAndRetainsIdentity -count=1
```

It verifies real acknowledgement, stored payload/subject/ID, retry deduplication
and unchanged Core publication using the unmodified standard stream config.

## Calendar policy

The operator installation registry accepts `organization_timezones`, mapping
installed organization IDs to IANA timezone names. Missing entries normalize to
explicit `UTC`; host-dependent `Local` and invalid names are rejected. The event
response includes this effective setting. A plugin must persist a calendar-based
deadline when the relevant state transition occurs; changing installation policy
later must not silently move an existing deadline. This configuration is an
operator policy, not a claim that the current Core organization schema has a
timezone field.
