# ADR: bounded native plugin host

Status: plugin contract level `v1alpha` for first-party plugins; not a stable
partner SDK.
Date: 2026-09-28.

## Decision

Keep Module management and normal deployment ownership in the Orchestrator.
Add generic discovery and a request-bound gateway without compiling add-on
business code into the standard server. Separately packaged native services and
different-origin UI pages consume these contracts. One extension-aware Core
build is necessary; changing a plugin must not require subsequent Core builds.

This repository contains only generic contracts. Add-on business logic is
packaged and released separately from the Orchestrator.

## Implemented boundary

- `GET /orgs/{orgId}/plugins` discovers organization-scoped contributions.
- Discovery responses (`GET /orgs/{orgId}/plugins` and
  `GET /orgs/{orgId}/plugins/{pluginId}`) carry `contract_level: v1alpha`, the
  level of the plugin contracts this Core implements.
- `POST /orgs/{orgId}/plugins/{pluginId}/invoke` accepts a registered operation
  ID, bounded JSON body, path parameters and repeated query values. It never
  accepts an arbitrary backend URL or forwards cookies or bearer tokens.
- The gateway uses the existing authenticated Orchestrator actor context and
  checks organization read access. The receiving add-on must independently
  check resource-specific IAM. A gateway assertion does not grant Core rights.
- `shared/plugincontext` signs an exact method, request URI,
  body hash, actor, organization, plugin, API operation and request ID with
  Ed25519. Assertions last at most 60 seconds and bind the receiver audience.
  This is a versioned `v1alpha` envelope, not a general bearer token or
  JWT. It is not a replacement for production workload-token exchange.
- Mutations require durable idempotency at the receiver. Signature checking
  alone does not make a command single-use. The verifier requires the operation
  ID expected by the handler and, for writes, an Idempotency-Key header equal to
  the signed request ID. Receivers deduplicate `claims.RequestID`, not a separate
  caller-selected header. This prevents a changed header from creating a second
  identity for the same signed command.
- Core's external Module command owns one transaction for the complete
  reservation set, multi-Module promotion or predecessor restoration, state
  update and receipt. A plugin uses stable request IDs to reconcile a lost
  acknowledgement. It never pretends that its database joins that transaction.
- Reservation epochs have fencing values. Release and reacquisition invalidate
  old epochs. Authorization must be checked before every receipt replay.

## Configuration

### Original-approver decisions

The existing authorization-check Error contract distinguishes the grant's
comparison subject (`plugin_subject_forbidden`, HTTP 403) from a controller,
installation or scope rejection. This code is returned only after those checks
succeed and IAM definitively rejects the exact subject with HTTP 401/403.
Availability failures remain HTTP 503 `authorization_unavailable`; a boolean
denial of controller authority is not an original-approver invalidation.

Deployment admission and its final callback distinguish original-approver loss
as HTTP 403 `plugin_approval_forbidden`, including inside a DP invocation
envelope. This is not a no-effect receipt. Clients must preserve the exact
pending request and withdraw/reconcile its existing barrier before invalidating
unused coverage. A committed receipt still consumes the approval regardless of
later IAM changes. No endpoint, permission, request field or generated DTO is
added, and controller checks plus final admission revalidation remain mandatory.

`PLUGIN_HOST_ENABLED` defaults to false. Enabling it requires an absolute
`PLUGIN_SIGNING_KEY_FILE` path. The key is an operator-owned PKCS8 Ed25519 PEM
file and must not be committed. The add-on receives only signed assertions,
never this key. Installations, organization enablements and their registered
operations are Plugin Manager state in Core's database, changed only through
the platform-scoped operator API; see
[adr-plugin-manager.md](adr-plugin-manager.md).

Origins require HTTPS unless the operator explicitly selects
`PLUGIN_DEVELOPMENT_HTTP=true`. Backend addresses are not returned by discovery.
The Console must independently reject a UI origin equal to the host origin.

Availability states are derived from the installation, its organization
enablement and the signed licence; they are never set by hand and are **not**
commercial entitlement verification by themselves. No enablement is
represented as proof of a licence. Disabled/unentitled states permit
registered read and safety operations but reject new forward work;
unavailable/incompatible states fail closed. Signed licence states, the grace-period warning and the rule that
licence material never stops Core are described in
[plugin-entitlements.md](plugin-entitlements.md).

The existing gateway trust boundary still applies: Core's authenticated `From`
context is supplied by platform authentication, not an Internet client's raw
header. Do not expose a CP test listener directly as a public authenticated API.

## Persistence

Migration 33 adds generic operation and receipt tables. Its idempotent
Resource Type declaration column statements are no-ops after migration 26.
Versions 27 to 32 are intentionally unused; new migrations continue after the
current maximum. Real PostgreSQL tests cover fresh schemas.

Migration 34 persists generic immutable operation grants, append-only command
receipts and exact dispatch records. Configured, distinct plugin/DP service
identities guard the public service endpoints. Grant preparation verifies an
original request-bound assertion, declared action/path constraints and current
IAM permission. Core captures Default predecessors and exact target UUIDs;
activate/suspend/release rotate fences. Module operations reauthorize receipt
replay, bind both reservation and grant fences and cannot restore arbitrary
history. Read-only recovery never renews authority. DP checks only already
recorded exact dispatches and uses its own audience, not a replay of a
gateway-to-plugin assertion. Full-stack execution qualification is still pending.

Activation is fresh scoped reauthorization of the same frozen intent, including
an already active grant. It binds the requesting actor and increments the grant
fence through exact revision comparison. Forward activation requires current
forward entitlement; only a verified signed safety source can retain recovery
authority in the immutable activation receipt. That authority belongs to the
exact current actor and fence, never to an old or missing receipt. Safety
reservation may reacquire only this operation's previously reserved exact Module
set, or retain its exact still-held leases without changing their Module fence.
Missing or foreign leases cannot be replaced by an ensure-held request. Old
worker fences cannot dispatch, but an exact dispatch admitted before fence
rotation can still settle against its immutable completion receipt.

`POST /orgs/{orgId}/plugins/{pluginId}/inventory` provides a bounded
repeatable-read metadata projection after delegated resource checks. It omits
Module source and input/provider payloads. Deployment readiness, effective-base
snapshots and retained encrypted material remain DP-owned. Plugin memory and
storage must not receive raw secret-bearing manifest/graph parameters merely
to draw a plan. Use value-free structural projections and an authoritative
fingerprint/reference; rollback is performed by DP from retained exact bases.
`GET /orgs/{orgId}/plugins/{pluginId}/operations/{operationId}/inventory`
returns the same projection unattended, for exactly an operation's frozen scope,
when that operation delegates `inventory.read` and may still reconcile (active,
suspended, or released within one journal retention window).

`GET /orgs/{orgId}/module-versions/proposed` is independent of plugins and
entitlements. It excludes archived Modules and returns immutable identity and
lifecycle metadata, including an explicitly empty optional digest.

Console discovery separates closed Core SDK operations from plugin operations.
Module contextual contributions are data-only relative routes with allowlisted
context substitutions and lifecycle predicates. They require full availability
and an advertised plugin operation; they do not grant mutation authority.

## Remaining gates

Independently authorized Core/DP service calls, generic deployment leases and
retained encrypted evidence, dynamic IAM registration, events, complete existing
UI/API acceptance, signed/offline installation and a fenced single-writer
migration rehearsal.

## Local browser component harness

`cmd/plugin-protocol-harness` exercises the real registry and Gateway on loopback
using an explicit synthetic org/actor and exact-origin CORS. It serves one
plugin from its registration manifest (`-manifest`, `-backend`, `-ui-origin`) as
available to that organization. It is for inspecting
the separately served UI and delegated protocol without booting the full
CP/IAM/DP/NATS/Vault stack. It must not be used as a production listener or
counted as end-to-end IAM verification. It does not supply fake successful Core
operations to the plugin.

No second Runner implementation, direct plugin Core-database access, secret
decryption shortcut is part of the plugin host.
