# ADR: database-backed Plugin Manager

Status: plugin contract level `v1alpha` for first-party plugins.
Date: 2026-10-07.

## Context

Plugin installation state decides which plugin backend Core routes to, which
service identity may act for a plugin, which organizations a plugin serves and
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
| Package | Immutable registration manifest of one plugin version (identity, migration generation, capability declarations, operations, UI contribution, service grants, authorization policies, entitlement declaration), keyed by the SHA-256 of the package manifest. |
| Installation | Core-assigned immutable installation UUID, served package and the target of a pending upgrade, state (`installed`, `active`, `disabled`, `uninstalled`, `purged`), backend and UI origins, two IAM platform principals (the plugin's service user and the admission checker), and the organization mode (`selected` or `all`) with the default timezone and approval policies of the enablements Core creates. |
| Status observation | What Core last read from the running plugin's status endpoint. A fact the plugin reports, not operator state. |
| Enablement | One organization of an installation: `enabled` or `disabled`, IANA timezone, deployment approval policies. An installation has one per organization it serves. |
| Registration | The contributions an active installation serves, materialised from its package on activation. A UI route prefix belongs to one installation; operations and capabilities live in the plugin's own namespace. |
| Audit event | One per committed change, with actor, reason, request ID, idempotency key digest, state before and after. Never updated or deleted. |

### Operator API

Platform-scoped routes, outside any organization:

| Route | Effect |
|---|---|
| `GET`, `POST /plugin-packages` | List; register a package. The same digest with the same manifest changes nothing. |
| `GET`, `POST /plugin-installations`, `GET /plugin-installations/{id}` | List and read; install a package. Core assigns the UUID. A plugin has at most one installation. |
| `PUT /plugin-installations/{id}/identity` | Record the service user and admission checker before activation; both must be IAM platform principals, the service user bound to the installation. |
| `POST /plugin-installations/{id}/actions/{activate,disable,enable}` | Serve the package registrations once the running plugin proves the package, or complete a pending upgrade; keep the installation safety-only for every organization; return it to active. |
| `POST /plugin-installations/{id}/upgrade` | Request the package the installation serves next. |
| `POST /plugin-installations/{id}/actions/{uninstall,reinstall}` | Remove registrations and enablements and keep the UUID and plugin data; return the retained installation to installed. |
| `DELETE /plugin-installations/{id}` | Purge an uninstalled installation and retire its UUID. |
| `PUT /plugin-installations/{id}/organization-mode` | Serve selected organizations or all of them, with the defaults of the enablements Core creates. |
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

The identities of an installation are IAM platform principals: service users
that belong to no organization and can hold no role. Core records a service
user for an installation only when IAM binds a plugin workload to exactly that
plugin ID, installation UUID and service user
(`GET /internal/plugin-workload-bindings`); otherwise it answers
`409 plugin_workload_binding_missing`. A binding of a service user of an
organization, which IAM kept from before platform principals, is refused with
`409 plugin_service_user_not_platform_principal`: every organization the
installation serves would depend on that one organization, whose removal
expires its service users' tokens, and its roles would travel with the plugin.
The admission checker must be a platform principal with a static token and no
workload binding (`GET /platform-principals/{id}`); otherwise Core answers
`409 plugin_admission_checker_not_platform_principal`. The operator therefore
creates the workload binding with its platform principal, and the checker,
first. Core asks IAM as its internal system identity before the command's
transaction, on every request including a replayed idempotency key, and
changes nothing when IAM refuses or cannot answer; the checks inside the
transaction are unchanged. Neither the scope nor the credential of a platform
principal ever changes, so the request-time check stays true.

At request time Core recognises both identities by equality with the
installation record: IAM authenticates a platform principal like any service
user and adds no organization. Enabling, changing and removing an
organization's enablement never ask IAM about the identities, so the safety
lever does not depend on IAM.

### Several organizations per installation

One installation, with one plugin runtime, one workload binding and one
platform principal, serves every organization it is enabled for. Each
enablement keeps its own state, timezone and deployment approval policies;
an approval in one organization is checked only against that organization's
policies. Changing or removing one organization's enablement leaves the
others as they are.

Core keeps the organizations apart where it decides. Every plugin route is
below `/orgs/{orgId}`, every Core effect re-verifies the enablement of that
organization in its own transaction, and all Core-side plugin state
(operation grants, dispatches, Module reservations and receipts, published
events, journal consumers, entitlement bindings) is keyed by organization and
plugin. A grant, cursor, Pin receipt or event of one organization is
therefore not found in another, and a gateway delegation names the
organization of its request. A licence grant names one organization of the
installation, or none: an installation-wide grant covers every organization
that has no grant of its own ([plugin-entitlements.md](plugin-entitlements.md)).

The security statement for `v1alpha`: a compromised plugin reaches every
organization its installation is enabled for, within the plugin boundary
(manifest service grants, reads within active operations, dispatch within a
frozen operation scope, event publication; new operations still need a fresh
user delegation), and all tenants share the plugin's own database. The
principal holds no IAM authority in any organization. Disabling one
organization is the per-organization kill switch; revoking the workload
binding or disabling the installation stops all of them.

### Serving all organizations

An installation's organization mode is `selected` (the operator enables
each organization) or `all`. Setting `all` enables, in that command, every
active organization that has no enablement of the plugin; afterwards Core
creates the enablement of every new organization inside the organization's
creation transaction (`createOrganizationCore`), with the journal of a
resource event subscriber starting there too. Both use the installation's
default timezone and approval policies, which apply only to enablements Core
creates from then on. An automatic enablement is one Plugin Manager change by
Core's internal system identity: it advances the revision and writes an audit
event whose reason names the organization and whose idempotency key digest is
the digest of `organization-created:<organization UUID>`. Right after the
organization exists for good (after its founder was added), Core registers
the capabilities of each such installation there, as after an enablement.

Core never changes an existing enablement on its own. A disabled enablement is
the organization's opt-out and stays disabled across mode switches,
default changes, new organizations and restarts; only the operator enables it
again. Switching back to `selected` keeps every enablement as it is, and
organizations created afterwards are no longer enabled. While the mode is
`all` every active organization keeps an enablement, so removing one is
refused with `409 plugin_serves_all_organizations`. Only an active or disabled
installation can serve all organizations; uninstall removes every enablement,
opt-outs included, and returns the installation to `selected`. An
organization created while the plugin host was off gets no enablement; the
installation lists it in `pending_organizations` until the operator sets
`all` again, which enables it.

A creation and a switch to `all` serialise on the Plugin Manager lock, so
whichever commits second sees the other: every organization is enabled by
exactly one of them. If the creation fails after its commit (IAM cannot add
the founder), Core removes the organization's enablements, again as audited
changes by its internal identity, and deletes the organization and its empty
journal state in one transaction.

### Capabilities in IAM

IAM keeps a catalogue of plugin capabilities per organization and plugin and
denies every check of a plugin capability it does not hold for the
organization at the checked scope. Core registers the capability
declarations of the package an installation serves for every organization
the installation is enabled for, through IAM's internal listener as its
internal system identity (`PUT /orgs/{orgId}/plugins/{pluginId}/capabilities`):

| When | Organizations |
|---|---|
| Right after an organization is enabled, also by Core when it is created | That organization |
| Right after an activation, also one that completes an upgrade | Every enabled organization |
| At start-up and every minute, on every replica | Every enabled organization of every installation |

A reconciliation first reads the organization's permissions
(`GET /orgs/{orgId}/permissions`) and replaces the plugin's catalogue only
when it differs, so repeating it costs IAM one read and no policy reload.
Reconciliations of one plugin take turns under a PostgreSQL advisory lock
and read what to register after taking it, so an older package never
overwrites a newer one. A command never waits for IAM: the enablement or
activation stands whatever IAM answers, and a failure is recorded and
retried by the next reconciliation.

Every enablement shows its `capability_registration`: `registered` once Core
registered or confirmed the served package's declarations, `pending` until
Core reconciled the served package for that organization (for example right
after an upgrade switched it), and `failed`, with the reason, when the last
attempt for the served package failed. The response of the enabling command
shows the state as of the command; a read right after it shows the result.
Like a status observation, the record is a fact about IAM and is kept outside
the revision and the audit.

Disabling an organization keeps its catalogue: the plugin's safety work there
(cancel, rollback, reconciliation) still needs IAM to check the original
actor's capabilities. Removing an enablement or uninstalling keeps it as
well: without an enablement Core refuses every effect of the plugin in that
organization, so the catalogue grants nothing, the roles that name the
capabilities stay readable, and enabling the organization again needs no
round trip to IAM. Core never unregisters a plugin's capabilities.

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

Registrations never collide. Core routes an operation only below its plugin
ID (`/orgs/{orgId}/plugins/{pluginId}/operations/{operationId}`), and a plugin
has at most one installation that is not purged, so equal operation IDs of
different plugins never meet; a package declares each operation once and none
in Core's host namespace (`core.`, `iam.`). A package declares only
capabilities in its own namespace, `plugin.<plugin ID>.<name>` with a
one-segment name, each once, and declares each completely, as IAM keeps it:
display name, description, category, risk level (`read`, `write`,
`manage`) and the scopes a role may grant it on (`organization`, `project`,
`environment`). A UI route prefix has one spelling, a slash
followed by lowercase segments without a trailing slash, as the Console
matches it, and belongs to one installation: an activation that would serve a
prefix another installation serves, also the activation that completes an
upgrade, is refused with `409 plugin_registration_conflict` and changes
nothing; a pending upgrade leaves the predecessor served. Uninstall releases
the prefix. Nested prefixes are no collision; the Console routes to the
longest match.

`upgrade` records the target package: a later version of the same plugin
that does not lower the migration generation and that Core can serve with the
installation's origins, identities, approval policies and default approval
policies. Core keeps serving
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
enablements (their journals pause with a gap), returns the organization mode
to `selected` and keeps the UUID, package, identities and the plugin's own
data. `reinstall` returns that installation,
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

### Package trust

Core verifies no package signature. The plugin runtime verifies the signed
package against the operator's publisher trust, and Core binds activation,
upgrade, every effect and the licence binding to the package digest the
running plugin reports; an operator holding `plugin_manager_write` is trusted
to register the registration manifest that belongs to a digest. The decision
and its limits are in [adr-plugin-package-trust.md](adr-plugin-package-trust.md).

## Not yet implemented

- Core reads plugin status only on activation; it does not observe the health
  of an active installation, and per-organization `unavailable` and
  `incompatible` still come only from the licence.
- Uninstall does not yet ask the plugin whether it is ready to decommission,
  and removing a single organization's enablement does not wait for that
  organization's safety work.

## Consequences

- No plugin installation file exists; installation, enablement and licence
  changes take effect without restarting Core.
- Read decisions may lag a change by one refresh interval; effects never do.
- Writes serialise globally; the Plugin Manager is an operator interface, not a
  high-rate path.
- Plugin releases must keep serving their predecessor's registrations during
  a rolling update, and the operator activates once the runtime rolled out.
- A downgrade below this schema is refused once an audit event exists, below
  the purged state once an installation is purged, and below several
  organizations per installation while one is enabled for more than one.
- A plugin that serves several organizations keeps every tenant's rows in its
  own database apart; Core refuses every effect across organizations, but it
  cannot see inside the plugin's database.
