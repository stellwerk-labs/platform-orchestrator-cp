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
| `PLUGIN_ENTITLEMENT_BUNDLE_FILE` | Signed grants and revocation snapshots to import at start-up. |

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

## Start-up: what can stop Core

A plugin failure degrades only that plugin, and an expired or missing licence
never affects Core functionality. Core therefore never refuses to start because
of entitlement material:

- the trust file is missing, unreadable, a symlink, too large, malformed, or of
  the wrong trust class for the mode (for example development trust in
  production mode);
- the bundle is configured without a trust file, or is missing, unreadable or
  malformed;
- a grant is a development grant in production mode, belongs to another or a
  removed installation or to another organization, is signed by an unpinned
  key, is tampered with, or conflicts with an already imported grant;
- a revocation snapshot fails verification or import.

Every signed document stands on its own signature: a rejected one is skipped
and the others in the bundle are still imported. Skipping is safe because an
unverifiable revocation revokes nothing, and a binding without a newly
imported grant keeps its persisted decision, which Core verifies again on every
use. Each problem is logged at error level as
`Plugin entitlement material was not applied; affected plugins stay unentitled`
with a stable `diagnostic` field:

| `diagnostic` | Cause |
|---|---|
| `plugin_entitlement_trust_rejected` | The trust file could not be loaded or parsed for this mode. |
| `plugin_entitlement_bundle_rejected` | The bundle could not be read or parsed, no trust is configured, or installed bindings could not be resolved. |
| `plugin_entitlement_grant_rejected` | A grant (by index) failed verification, has no exact installed binding, or was not imported. |
| `plugin_entitlement_revocations_rejected` | A revocation snapshot (by index) failed verification or import. |

The grant detail is deliberately coarse and does not say which binding or which
check failed. The affected plugin reports `not_entitled` (no grant) or
`untrusted` (persisted grant no longer verifies) in discovery, refuses forward
work with `plugin_not_entitled`, and keeps read, export, cancel and rollback for
features activated under a grant. Core-only operations are unaffected.

Core configuration that is not entitlement material still fails fast, because
it describes Core itself and affects every plugin equally: an invalid
`PLUGIN_ENTITLEMENT_MODE`, the plugin installation registry
(`PLUGIN_INSTALLATIONS_FILE`, including its entitlement declarations), the
plugin assertion signing key (`PLUGIN_SIGNING_KEY_FILE`), the plugin CA bundle
(`PLUGIN_CA_FILE`), the fixed Data Plane plugin port and
`PLUGIN_EVENT_JOURNAL_RETENTION`.

Uninstalling a plugin or removing an organization therefore no longer requires
removing its grant from the bundle first; the stale grant is skipped with
`plugin_entitlement_grant_rejected`.
