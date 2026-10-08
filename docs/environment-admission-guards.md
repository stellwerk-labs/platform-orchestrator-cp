# Environment admission guards

Environment admission guards are a coordination boundary between
the ordinary Data Plane Deployment path and Core-owned Module Version Pins.
They do not execute infrastructure and are not a replacement Deployment engine.

## Authority and network boundary

Only the operator-configured Data Plane service identity may authorize, settle,
or seal an admission. Core authenticates that identity through the established
gateway and rejects plugin backend identities. Deploy and rollback admissions
also recheck the initiating actor's current `deployment_write` permission.
Destroy is limited to the existing authenticated Environment-deletion workflow
and requires Core's Environment to be deleting.

DP prepares a durable request before contacting Core. Core records a retained
guard and advances the Environment admission epoch while holding the same
Environment advisory lock used by Pin writes. DP then binds the positive Core
epoch to the ordinary Deployment in its existing database transaction. Core
never calls DP while holding this transaction or lock.

An ambiguous authorization result is not treated as absence. DP first seals a
permanent local absence tombstone under its admission lock, then calls Core's
`seal-absence` operation with the complete original intent. Core serializes the
tombstone with a delayed authorize request. A committed Deployment is settled
only from DP's durable terminal state.

## Exact intent and replay

The request contains immutable organization, project, Environment, actor and
mode identity, the resolved Module UUID-to-Version UUID graph, and a digest of
the complete compiled DP intent. DP's digest covers the manifest, resource
graph, generated Tofu, full artifact metadata, effective base, rollback target,
runner settings and encryption recipients. Values remain in DP; only the digest
crosses the boundary.

Two optional evidence fields preserve existing Module lifecycle semantics:

- `carry_forward_module_versions` is an exact subset of the target graph that
  DP derives from the successful effective-base graph. It permits an archived
  Module to retain only its already-active Version while other Modules change.
- `confirmed_restricted_version_uuids` contains only Defective target Versions
  actually used by this compilation and explicitly confirmed by the caller.

Nil and empty optional evidence canonicalize identically. Evidence is part of
the retained immutable intent, so an admission ID cannot be replayed with a
different actor, target, lifecycle exception, scope, mode, or digest. Destroy
admissions require all Module lifecycle evidence to be empty.

## Current-state revalidation

Core rechecks lifecycle state after taking the Environment lock, then locks
Modules and Versions in canonical Module UUID order. The decision matches the
normal catalogue boundary:

- archived Modules allow exact carry-forward or rollback only;
- Deprecated Versions require an exact active Pin or rollback;
- Defective Versions require exact confirmation, the current scoped
  `module.version.pin-defective` (or rollback-restricted) permission, and an
  exact active Pin or rollback;
- Proposed Versions require rollback, exact carry-forward, an exact active Pin,
  or a positive current `module.version.use-proposed` decision made by Core.

The Proposed IAM result is trusted in-memory policy, not client intent and not
persisted as authority. The locked model rechecks Pin state, including on an
active-guard replay, so removing a Pin between the API's read and the model lock
cannot turn stale observation into permission.

Pin evidence readers capture the Environment admission epoch before remote DP
inspection and compare it under the same lock when writing. Every new ordinary
or external admission advances that epoch, including an admission that starts
and completes inside the inspection window. Unresolved guards remain barriers;
timeouts never release them.

## Configuration and limitations

`DEPLOYMENT_ADMISSION_IDENTITY` names the UUID of the Data Plane's admission
identity. That identity must be an IAM platform principal with a static token
(`POST /platform-principals` in IAM): it belongs to no organization and holds
no role. The Data Plane holds one token for every organization, so Core admits
for every organization as this one identity, including an organization created
while Core runs; neither a configuration change nor a restart is needed. Core
refuses to start when the value is not the canonical UUID of a service
identity, and refuses every callback while it is unset. Core looks the caller
up in IAM on every callback and refuses any other identity, such as a service
user of an organization or a plugin workload, with
`403 deployment_admission_service_forbidden`; when IAM cannot answer, the
callback fails with `503 authorization_unavailable`. DP requires both
`DEPLOYMENT_ADMISSION_CORE_GATEWAY_URL` and
`DEPLOYMENT_ADMISSION_CORE_BEARER_TOKEN_FILE`; configuring only one is a startup
error. With neither configured, the released ordinary path remains unchanged
and no Pin-race guarantee is claimed.

This is part of the `v1alpha` plugin host contract. It requires coordinated Core
and DP revisions plus gateway-authenticated internal listeners. It is not a
published partner SDK, a general delegation token, or evidence that a complete
plugin acceptance suite has passed.
