# Plugin entitlements: availability, grace and start-up

Core verifies offline signed licences (COSE_Sign1 grants) for plugins whose
package declares `entitlement.mode: required`. This page describes what each
licence state allows, how clients see the grace warning, and which operator
input can stop Core at start-up. For development licences see
[development-entitlement-tool.md](development-entitlement-tool.md).

## Configuration

| Variable | Meaning |
|---|---|
| `PLUGIN_ENTITLEMENT_MODE` | `production` (default) or `development`. Selects which trust class Core accepts. |
| `PLUGIN_ENTITLEMENT_TRUST_FILE` | Pinned issuer public keys (strict JSON, regular file, no symlink). |

Platform operators import signed grants and revocation snapshots with
`POST /plugin-entitlement-bundles` (platform permission `plugin_manager_write`,
see [adr-plugin-manager.md](adr-plugin-manager.md)). The request carries the
exact bundle file bytes, base64-encoded. An import applies without a restart
and reports how many documents it imported, with one diagnostic per skipped
document. An organization grant matches only the binding (installation UUID,
plugin, organization) of an installation enabled for that organization; an
installation-wide grant matches the installation UUID and plugin of an
installed, active or disabled installation, also before any organization is
enabled. Upgrade, disable, uninstall and reinstall keep the installation UUID,
so imported grants keep applying once the installation serves the
organization again; a purge retires the UUID and its grants never match
again.

## Grant scope

A grant either names one organization (claim 8) or none. Without an
organization it is installation-wide: it covers every organization of the
installation that has no grant of its own, including organizations created
later. For an organization with its own grant, that grant decides alone, with
its features, validity and revocation; the installation-wide grant does not
fill in for a feature it lacks or for its expiry. Feature activations stay per
organization, under whichever grant covers it. A grant with a higher sequence
replaces the current grant of the same binding: plugin, installation and
organization for an organization grant, plugin and installation for an
installation-wide grant.

An installation-wide grant may carry the limit `organizations` (claim 10, 1 to
2^63-1), the number of organizations the installation may enable.

Reads show which grant covers an organization. Discovery and the plugin's own
preflight carry `entitlement.grant_scope` (`organization` or `installation`,
absent without a grant). The Plugin Manager's installation read carries, for a
package that requires a licence, `entitlement.installation_grant` and
`entitlement.enabled_organizations`, and on each enablement the covering
`entitlement_grant` with issuer, issuance, sequence, features, limit, validity
and its state under the current trust.

## Feature states

Each declared feature has one effective state per organization. Discovery
returns it as `entitlement.features.<feature>.state`.

| State | Forward work | Read, export, cancel, rollback |
|---|---|---|
| `valid` | allowed | allowed |
| `grace` | allowed, with a warning | allowed |
| `expired`, `revoked`, `untrusted`, `not_yet_valid` | refused, `409 plugin_not_entitled` | allowed if the feature was activated under a grant |
| `not_entitled`, `missing` (no grant for this binding) | refused, `409 plugin_not_entitled` | refused |

`untrusted` means a persisted grant no longer verifies against the current
trust file, for example after a key was removed or the mode changed. Expiry is
monotonic: after Core has observed `grace_until`, moving the clock back does
not restore forward work.

## Grace warning

While a feature is in `grace`, forward work still succeeds and Core adds a
machine-readable warning. The `code` is stable (`plugin_entitlement_grace`);
`message` is display text; `grace_until` is the earliest grace end among the
listed `features`. The same `PluginEntitlementWarning` object appears in:

| Where | Field |
|---|---|
| `GET /orgs/{orgId}/plugins` (contributions) | `items[].entitlement.warning` |
| `GET /orgs/{orgId}/plugins/{pluginId}` (capability) | `entitlement.warning`, and `admin_action` carries the message |
| `GET /orgs/{orgId}/plugins/{pluginId}/entitlement` (service preflight) | `entitlement.warning` |
| `POST /orgs/{orgId}/plugins/{pluginId}/invoke` | `entitlement_warning`, when the operation's required feature is in grace |
| `POST /orgs/{orgId}/plugins/{pluginId}/operations/{operationId}/deployments` | `entitlement_warning`, when the service action's feature is in grace |

The warning never changes authorization. Clients (Console, CLI, Terraform
provider, plugins) should show `message` and `grace_until` wherever they show
the operation's result. After `grace_until` the feature is `expired` and forward
work is refused as in the table above.

## Start-up and import: what can stop Core

A plugin failure degrades only that plugin, and an expired or missing licence
never affects Core functionality. Core therefore never refuses to start because
of entitlement material, and an import never fails because of one document:

- the trust file is missing, unreadable, a symlink, too large, malformed, or of
  the wrong trust class for the mode (for example development trust in
  production mode);
- a bundle is imported without configured trust, or is malformed;
- a grant is a development grant in production mode, belongs to another or a
  removed installation or to another organization, is signed by an unpinned
  key, is tampered with, or conflicts with an already imported grant;
- a revocation snapshot fails verification or import.

Every signed document stands on its own signature: a rejected one is skipped
and the others in the bundle are still imported. Skipping is safe because an
unverifiable revocation revokes nothing, and a binding without a newly
imported grant keeps its persisted decision, which Core verifies again on every
use. A trust problem is logged at start-up at error level as
`Plugin entitlement material was not applied; affected plugins stay unentitled`
with a stable `diagnostic` field; an import returns its problems in
`diagnostics[].code`:

| `diagnostic` | Cause |
|---|---|
| `plugin_entitlement_trust_rejected` | The trust file could not be loaded or parsed for this mode. |
| `plugin_entitlement_bundle_rejected` | The bundle could not be parsed or no trust is configured. |
| `plugin_entitlement_grant_rejected` | A grant (by index) failed verification, has no exact installed binding, or was not imported. |
| `plugin_entitlement_revocations_rejected` | A revocation snapshot (by index) failed verification or import. |

The grant detail is deliberately coarse and does not say which binding or which
check failed. The affected plugin reports `not_entitled` (no grant) or
`untrusted` (persisted grant no longer verifies) in discovery, refuses forward
work with `plugin_not_entitled`, and keeps read, export, cancel and rollback for
features activated under a grant. Core-only operations are unaffected.

Core configuration that is not entitlement material still fails fast, because
it describes Core itself and affects every plugin equally: an invalid
`PLUGIN_ENTITLEMENT_MODE`, the Plugin Manager refresh interval, the first load
of the Plugin Manager state, the plugin assertion signing key
(`PLUGIN_SIGNING_KEY_FILE`), the plugin CA bundle (`PLUGIN_CA_FILE`), the fixed
Data Plane plugin port and `PLUGIN_EVENT_JOURNAL_RETENTION`. An installation
whose stored state no longer validates is not served and is logged; the
others are.

Uninstalling a plugin or removing an organization therefore no longer requires
removing its grant from the bundle first; the stale grant is skipped with
`plugin_entitlement_grant_rejected`.
