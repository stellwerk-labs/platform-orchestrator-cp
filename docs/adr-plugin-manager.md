# ADR: database-backed Plugin Manager

Status: plugin contract level `v1alpha` for first-party plugins.
Date: 2026-10-07.

## Context

Plugin installation state decides which plugin backend Core routes to, which
service identity may act for a plugin, which organization a plugin serves and
which organization's resource events Core journals. That state needs one
authority that changes without restarting Core, records who changed what and
why, and stays correct when several operators and several Core replicas act at
the same time.

## Decision

The Plugin Manager state in Core's database is the only source of plugin
installation truth. Helm and the plugin runtime chart reconcile workloads only
and never write it.

| Resource | Content |
|---|---|
| Package | Immutable registration manifest of one plugin version (identity, operations, UI contribution, service grants, authorization policies, entitlement declaration), keyed by the SHA-256 of the package manifest. |
| Installation | Core-assigned immutable installation UUID, package, state (`installed`, `active`, `disabled`), backend and UI origins, service user and admission checker. |
| Enablement | One organization of an installation: `enabled` or `disabled`, IANA timezone, deployment approval policies. In `v1alpha` an installation has exactly one. |
| Registration | The contributions an active installation serves, materialised from its package on activation. A UI route prefix belongs to one installation. |
| Audit event | One per committed change, with actor, reason, request ID, idempotency key digest, state before and after. Never updated or deleted. |

### Operator API

Platform-scoped routes, outside any organization:

| Route | Effect |
|---|---|
| `GET`, `POST /plugin-packages` | List; register a package. The same digest with the same manifest changes nothing. |
| `GET`, `POST /plugin-installations`, `GET /plugin-installations/{id}` | List and read; install a package. Core assigns the UUID. A plugin has at most one installation. |
| `PUT /plugin-installations/{id}/identity` | Record the service user and admission checker before activation. |
| `POST /plugin-installations/{id}/actions/{activate,disable,enable}` | Serve the package registrations; keep the installation safety-only for every organization; return it to active. |
| `PUT`, `DELETE /plugin-installations/{id}/organizations/{orgId}` | Enable or disable for the organization (disabled is safety-only); remove the enablement. |
| `POST /plugin-entitlement-bundles` | Import signed grants and revocations ([plugin-entitlements.md](plugin-entitlements.md)). |
| `GET /plugin-audit-events` | Page the audit trail. |

Every write takes an `Idempotency-Key`, the `expected_revision` of the Plugin
Manager the operator decided on, and a reason. It runs in one transaction that
serialises with every other write, replays the stored response for a known key
(a different request under that key is `409 plugin_manager_idempotency_conflict`),
refuses another revision with `409 plugin_manager_revision_conflict`, and on a
change advances the revision and writes one audit event. A step whose target
state already holds changes nothing and writes no audit event, so an
interrupted installation resumes by running its steps again.

Holders of the platform permissions `plugin_manager_read` and
`plugin_manager_write` are configured users (`PLUGIN_MANAGER_READERS`,
`PLUGIN_MANAGER_WRITERS`; writers also read). Plugin workloads authenticate as
IAM service users, and a service user never holds either permission, even when
configured; Core refuses such a configuration at start-up. Organization roles
are never consulted. With the plugin host disabled every route returns
`404 plugin_host_disabled`; Core-only operation is unaffected.

### Propagation and authority

Each replica serves read decisions (discovery, routing, UI, descriptors) from
an immutable snapshot. It polls the revision every
`PLUGIN_MANAGER_REFRESH_INTERVAL` (default 2 seconds, 100 milliseconds to one
minute) and reloads on change; the replica that executed a command reloads at
once. A replica serves plugins only after its first load.

The snapshot is never authority for an effect. Every Core effect performed for
a plugin re-verifies, inside its own transaction, that the installation UUID
and package digest the snapshot named are still served and the installation
and organization enablement are active; safety work stays permitted while
either is disabled. The rows stay share-locked until the effect commits, so a
concurrent change either waits for the effect or the effect observes it.
Resource journal reads check the subscription in the snapshot they read, and
the first licence feature activation checks the enablement before it is
recorded.

Availability is derived, never configured: `available` while the installation
is active and the organization enabled, `not_enabled` (safety-only) while
either is disabled, then refined by the signed licence.

### Resource event journal

An organization is journaled exactly while a plugin is enabled for it (in
either enablement state) with a registered `resource-events.read` grant. The
command that changes an enablement or registrations starts or pauses the
journal in its own transaction; journal writers read the decision under a
shared per-organization lock at commit. Resuming marks a gap, so cursors issued
before it fail with `plugin_resource_cursor_expired`. A plugin that is no longer
subscribed cannot read the journal. Retention counts the operations of
currently subscribed plugins as consumers. A server started with the plugin
host disabled pauses every journal; a server with the host enabled
re-establishes the journal state from the subscriptions before it serves.

### Package trust in `v1alpha`

Core does not verify package signatures. The plugin runtime verifies the signed
package against the operator's publisher trust before it starts the plugin
process. Core records the package manifest digest with the registration
manifest, binds the installation, its entitlement grants and every effect check
to that digest, and is to bind activation to the digest the running plugin
reports. An operator holding `plugin_manager_write` is therefore trusted to
register the manifest that belongs to a digest.

Verifying in Core as well would duplicate the runtime's verifier and its trust
distribution without adding a check the runtime and the reported digest do not
already provide for first-party plugins. Partner publishers need Core-side
verification and revisit this decision.

## Not yet implemented

- Activation does not yet require the running plugin to report the recorded
  installation UUID, package digest and migration generation, and Core does
  not probe plugin health; `unavailable` and `incompatible` come only from the
  licence.
- Upgrade, uninstall, reinstall and purge.
- Verifying service identities against IAM workload bindings and the service
  user's organization, and holding the platform permissions in IAM.
- Collision checks beyond the UI route prefix.

## Consequences

- No plugin installation file exists; installation, enablement and licence
  changes take effect without restarting Core.
- Read decisions may lag a change by one refresh interval; effects never do.
- Writes serialise globally; the Plugin Manager is an operator interface, not a
  high-rate path.
- A downgrade below this schema is refused once an audit event exists.
