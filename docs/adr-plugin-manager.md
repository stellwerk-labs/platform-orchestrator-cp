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
| Package | Immutable registration manifest of one plugin version (identity, migration generation, operations, UI contribution, service grants, authorization policies, entitlement declaration), keyed by the SHA-256 of the package manifest. |
| Installation | Core-assigned immutable installation UUID, served package and the target of a pending upgrade, state (`installed`, `active`, `disabled`, `uninstalled`, `purged`), backend and UI origins, service user and admission checker. |
| Status observation | What Core last read from the running plugin's status endpoint. A fact the plugin reports, not operator state. |
| Enablement | One organization of an installation: `enabled` or `disabled`, IANA timezone, deployment approval policies. In `v1alpha` an installation has exactly one, the organization of its service user. |
| Registration | The contributions an active installation serves, materialised from its package on activation. A UI route prefix belongs to one installation. |
| Audit event | One per committed change, with actor, reason, request ID, idempotency key digest, state before and after. Never updated or deleted. |

### Operator API

Platform-scoped routes, outside any organization:

| Route | Effect |
|---|---|
| `GET`, `POST /plugin-packages` | List; register a package. The same digest with the same manifest changes nothing. |
| `GET`, `POST /plugin-installations`, `GET /plugin-installations/{id}` | List and read; install a package. Core assigns the UUID. A plugin has at most one installation. |
| `PUT /plugin-installations/{id}/identity` | Record the service user and admission checker before activation; IAM must bind the service user to the installation and keep it in every organization the installation is enabled for. |
| `POST /plugin-installations/{id}/actions/{activate,disable,enable}` | Serve the package registrations once the running plugin proves the package, or complete a pending upgrade; keep the installation safety-only for every organization; return it to active. |
| `POST /plugin-installations/{id}/upgrade` | Request the package the installation serves next. |
| `POST /plugin-installations/{id}/actions/{uninstall,reinstall}` | Remove registrations and enablements and keep the UUID and plugin data; return the retained installation to installed. |
| `DELETE /plugin-installations/{id}` | Purge an uninstalled installation and retire its UUID. |
| `PUT`, `DELETE /plugin-installations/{id}/organizations/{orgId}` | Enable or disable for the organization of the installation's service user (disabled is safety-only); remove the enablement. |
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

IAM decides who operates the Plugin Manager. The platform permissions
`plugin_manager_read` and `plugin_manager_write` are held by the users IAM is
configured with (`PLUGIN_MANAGER_READERS`, `PLUGIN_MANAGER_WRITERS`; writers
also read). Core asks IAM on every request (`POST /internal/authorize` with the
resource `platform`) and answers `403 plugin_manager_forbidden` when IAM
refuses. Plugin workloads authenticate as IAM service users: IAM does not start
when a service user is configured as a holder and never lets a plugin workload
hold a platform permission. Organization roles are never consulted. With the
plugin host disabled every route returns `404 plugin_host_disabled`; Core-only
operation is unaffected.

Core records a service user for an installation only when IAM binds a plugin
workload to exactly that plugin ID, installation UUID and service user
(`GET /internal/plugin-workload-bindings`); otherwise it answers
`409 plugin_workload_binding_missing` and changes nothing. The operator
therefore creates the IAM workload binding for the installation UUID first.
Core asks IAM before the command's transaction, on every request including a
replayed idempotency key; the checks inside the transaction are unchanged.

In `v1alpha` an installation serves exactly one organization, the
organization of its service user. IAM keeps every service user in exactly one
organization, so Core creates an enablement only for an organization in which
IAM lists the installation's service user (`GET /orgs/{orgId}/service-users`,
asked as the internal system identity) and otherwise answers
`409 plugin_service_user_organization_mismatch`; a second organization is
refused with `409 plugin_enablement_limit`. Identities are refused the same way
while the installation is enabled for an organization that is not the service
user's. Core asks IAM before the command's transaction; the command creates the
enablement only if the installation still names the service user IAM was asked
about, and otherwise answers `409 plugin_installation_state`. Changing an
existing enablement keeps its organization and never asks IAM, so disabling an
organization does not depend on IAM. An installation whose package declares no
service grants has no service user; it serves the one organization the operator
enables.

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

### Activation, upgrade and removal

A running plugin serves its status at `/.well-known/stellwerk/plugin-status`
on its backend origin (`shared/pluginstatus`): the installation UUID the
plugin runtime configured, the package digest the runtime verified before it
started the process, and the migration generation applied in the plugin's
database, read on every request. A package declares the generation it runs
on in its registration manifest; `/.well-known/` is reserved for Core and no
plugin operation routes there.

`activate` reads that status outside the serialised command, with the
operator's plugin CA and without following redirects, and records what it
observed. The command then serves the package registrations only when the
plugin reports exactly the installation UUID, the package digest (the target
of a pending upgrade, if any) and its migration generation. Otherwise it
changes nothing and answers `409` with a stable diagnostic:
`plugin_status_unreachable`, `plugin_status_invalid`,
`plugin_installation_mismatch`, `plugin_package_digest_mismatch` or
`plugin_migration_generation_mismatch`. Installation reads derive the
availability (`unavailable` until the first activation) and the same
diagnostic from the last observation. Because a refusal changes no revision,
the operator retries the same command once the plugin runtime caught up.
Observations are not audited: they never change what Core serves or
authorizes, and the activation that acts on one is.

`upgrade` records the target package: a later version of the same plugin
that does not lower the migration generation and that Core can serve with the
installation's origins, identities and approval policies. Core keeps serving
the current package, its registrations and every effect bound to its digest
while the plugin runtime rolls. The next `activate` that reads the target
digest and generation switches package and registrations in one
transaction, so a replica serves either package, never a mix. During the
rolling update the successor must serve the predecessor's registrations,
because Core routes those until the switch. Effects that a replica
authorized from its snapshot of the predecessor are refused with
`409 plugin_not_installed` for at most one refresh interval after the switch
and succeed on retry. The installation UUID, and with it the licence binding,
never changes; a disabled installation stays disabled. Downgrades are refused.

`uninstall` is refused with `409 plugin_work_unsettled` while Core holds
safety work of the plugin in any organization: operation grants not
released, dispatches not settled or Module reservations still active. There
is no force option. Until it succeeds the plugin's cancel, rollback and read
paths stay available; disabling first stops forward work. New work
share-locks the installation row that uninstall locks for update, so the
check cannot race it. Uninstall removes the registrations and organization
enablements (their journals pause with a gap) and keeps the UUID, package,
identities and the plugin's own data. `reinstall` returns that installation,
with its UUID, to `installed`; it is activated and enabled again like a new
one, and its licence grants match again. A plugin keeps at most one
installation that is not purged, so a new installation never shadows a
retained one. `purge` is a separate step that requires a prior uninstall and
retires the UUID for good; Core does not own plugin data, which the operator
deletes through the plugin runtime.

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
to that digest, and serves a package only after the running plugin reports
that digest. An operator holding `plugin_manager_write` is therefore trusted
to register the manifest that belongs to a digest.

Verifying in Core as well would duplicate the runtime's verifier and its trust
distribution without adding a check the runtime and the reported digest do not
already provide for first-party plugins. Partner publishers need Core-side
verification and revisit this decision.

## Not yet implemented

- The plugin runtime does not yet pass the installation UUID and verified
  package digest to the plugin process, and plugins do not yet serve the
  status endpoint.
- Core reads plugin status only on activation; it does not observe the health
  of an active installation, and per-organization `unavailable` and
  `incompatible` still come only from the licence.
- Uninstall does not yet ask the plugin whether it is ready to decommission,
  and removing a single organization's enablement does not wait for that
  organization's safety work.
- A dedicated IAM lookup of a service user's organization. Core pages through
  the service users of the organization being enabled, which IAM lets the
  internal system identity read in every organization.
- Collision checks beyond the UI route prefix.

## Consequences

- No plugin installation file exists; installation, enablement and licence
  changes take effect without restarting Core.
- Read decisions may lag a change by one refresh interval; effects never do.
- Writes serialise globally; the Plugin Manager is an operator interface, not a
  high-rate path.
- Plugin releases must keep serving their predecessor's registrations during
  a rolling update, and the operator activates once the runtime rolled out.
- A downgrade below this schema is refused once an audit event exists, and
  below the purged state once an installation is purged.
