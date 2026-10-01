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
There is no retention purge in this experiment. Migration 37 refuses a schema
downgrade that would discard event history. Application rollback keeps the
additive tables. Events before this migration are not retroactively synthesized;
initial planning still reads authoritative current resource inventory.

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
