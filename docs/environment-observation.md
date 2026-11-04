# Optional environment-observation/v1 boundary

The generated optional API is `shared/observationapi`, sourced from
`openapi/observation.yaml`. Its separate server package is
`internal/observationapi`. Existing shared/v3 mandatory interfaces do not gain
methods. Run and result DTOs match the Data Plane's optional API. Normal Core
operation does not require this protocol or a plugin.

Clients reach these routes through the existing authenticated gateway. The
plugin supplies its rotating workload Bearer token to that gateway. The existing
trusted `From` actor context inside CP is not an Internet authentication header;
do not expose a CP listener as an authenticated public API.

A manifest can declare `required_protocols: [environment-observation/v1]`.
Unknown protocol versions make discovery incompatible. A package requiring the
known protocol also depends on a signed, no-effect Data Plane capability probe
(bounded to2seconds, cached for2seconds). Missing/old/incompletely configured
Data Plane support makes only that plugin incompatible; Core startup and plugins
without the requirement do not depend on the probe. Operations and UI
contributions can declare `require_org_admin: true`; omission retains existing
behavior. Core checks authoritative canonical Admin membership before gateway
forwarding, delegated proof exchange and scoped grant creation. A namespaced
permission or platform privilege cannot substitute for Admin membership. IAM's
optional canonical administrator API is required; old/unavailable IAM fails
closed without affecting ordinary routes.

## Authority and reconciliation

`PUT .../observations/v1/grants/{grantId}` requires a fresh Admin proof and the
exact `observation.grant.replace` or `observation.grant.revoke` source service
action. A replace freezes explicit Project/Environment UUID pairs, rotates the
revision and authorizing actor, and carries no Module or mutation authority.
`GET` uses `observation.grant.get`, returns exact targets, actor, revision and
`last_effect_request_id`. It is scoped to the configured installation service
and read-safety entitlement fence, so configuration acknowledgements remain
recoverable after the former controller loses Admin membership. This metadata
read grants no admission authority. The fixed policies are
`last-successful-ordinary-deployment` and `full-plan-if-idle`.

Try-start requires an active current revision, exact target identity, current
Admin membership, enablement and forward entitlement. These checks and grant
locking cover the signed downstream request. Requests have UUID identities and
a bounded occurrence/admission window. Identical requests replay the existing
receipt; changed bodies conflict. Transport ambiguity retains a CP request
journal entry and must be resolved with the same request identity.

Exact previously accepted run/request metadata remains readable after selection
rotation, grant revocation or authorizing actor loss. Its immutable CP journal
scope, configured installation service identity, declared read action and
current installation/read-safety entitlement fence authorize that reconciliation.
No old revision can admit a new observation. These read ports expose only safe
retained metadata/results; Data Plane retention may report unavailable/expired.
A historical result can be `stale` after a newer trustworthy successful baseline,
or unavailable after an untrustworthy newer mutation. Neither is a clean current
observation. No raw config, state, plan, diagnostic, output or resource address
crosses this boundary.

Explicit remedial Deploy requires a fresh user proof, current canonical Admin,
normal Deployment permission and normal authoritative Core admission. It replays
an exact qualifying retained baseline, never an observation plan. CP persists
its exact proof-free attempt identity before sending it. Lost acknowledgements
are reconciled with `GET .../observations/v1/redeployments/{requestId}` and
`observation.redeploy.get`, never an old proof. Missing retained Deployment
history is unavailable, not successful or failed by inference.

## Target selection and Runner capacity

`POST .../observations/v1/targets/query` uses a fresh Admin proof and the declared
`observation.targets.query` service action. Project/type/production/include/
exclude filters run in SQL with keyset pagination (1..200, default 100), not an
unpaged inventory snapshot. Cursors bind organization, plugin and exact filter
body. An explicit empty Environment include set selects none. Production is the
canonical Environment Type flag, never a name heuristic. The plugin persists
explicit selected UUIDs; later-created resources are not enrolled automatically.

Every Runner configuration union accepts optional positive `max_parallel_jobs`
and `supported_protocols`. Omitting the limit preserves unlimited legacy
admission; configuration patches preserve an existing omitted value, including old-client
bodies that supply the complete legacy configuration. In PATCH, an explicit
`max_parallel_jobs: null` removes the limit (unlimited), and
`supported_protocols: null` removes protocol opt-in. An explicit protocol `[]`
stores an empty declaration. Null is rejected for these create fields. The typed
Runner configuration summary exposes both optional configured values. A lower
limit drains existing jobs before admitting more. The Data Plane owns persistent
reservations shared by all normal and observation jobs. Observation requires an
explicit finite limit and the assigned Runner's protocol opt-in. Discovery of a
Core protocol does not establish a particular Runner's support; target admission
returns unsupported when the complete assigned stack cannot execute it.

## Local qualification

Migration 52 owns separate grant, command receipt, observation request and
remedial attempt tables. Downgrade refuses live authority, unresolved requests
or retained accepted/remedial work. Core database credentials, NATS and raw
material read access are never supplied to the plugin.

The candidate was exercised with real isolated PostgreSQL schemas for exact
scope/CAS races, expired/stale fences, durable ambiguity, retained reads after
revocation, cross-tenant denial, canonical production filtering and pagination
through 10,002 Environments. This is source-level candidate evidence, not proof
of published SDK availability or packaged full-stack qualification.

## Dispatch durability and empty configuration

Observation and remedial dispatch use two transactions. The first validates
current authority and commits the immutable scoped routing intent before any
DP request. A separate transaction reacquires installation, enablement,
entitlement and current actor authority, then holds its effect fence across the
signed send. Observation also rechecks the exact active grant revision; remedial
Deploy re-verifies the fresh source assertion and ordinary Deployment permission.
A committed routing intent scopes retained reads and explicit seal recovery. It
never authorizes a later execution POST. Caller cancellation rolls back only the effect
transaction, leaving the already committed identity available for restart
recovery. No canceled-context commit is needed.

Revoking a missing grant at expected revision zero creates a revoked revision-one
tombstone with no targets or expiry. It has a normal exact command receipt and
confers no forward authority. A later nonempty replace uses expected revision
one. Replaying the original empty command returns its historical receipt without
changing a newer configuration.

The dispatch fault regression uses real isolated PostgreSQL and the real signed
HTTP transport with a peer that commits a durable receipt before dropping the
response. It covers caller cancellation, fresh serving-object recovery, closed
admission deadlines, revoked grants, removed Admin authority, exact-body conflicts
and safe eventual status. Controlled authority collaborators and the durable HTTP
peer are failure-injection tools, not evidence of actual IAM/DP/Runner execution.
The combined deployed positive gate and first empty plugin configuration remain
separate runtime qualification requirements.

## Seal-if-absent recovery and indexed scope

`environment-observation-recovery/v1` is a separate optional Core requirement.
A requiring plugin must declare it alongside `environment-observation/v1`.
An old DP advertising only the execution protocol does not activate a recovery
consumer. Old execution-only plugins retain their compatibility behavior. Runner
protocol opt-in continues to use only the execution protocol.

Two safety POST operations close an exact admission key:

- `.../requests/{requestId}/resolve`, action `observation.request.resolve`,
  accepts the original `ObservationRequest` after its admission deadline.
- `.../redeployments/{requestId}/resolve`, action `observation.redeploy.resolve`,
  accepts `{run_id, request: ObservationRedeployRequest}` with no proof. The
  caller explicitly abandons further submission of that exact remedial key;
  elapsed time alone never establishes that work did not execute.

Each typed resolution returns `{request_id, disposition, receipt?}`. `accepted`
requires the exact existing accepted receipt. `not_started` has no receipt or
fabricated deployment identity. A previously skipped observation also resolves
as `not_started`; its original GET receipt is unchanged. Accepted jobs retain
normal result/status reconciliation. Sealing never cancels a job, releases its
reservation, changes its status/baseline or clears a finding.

Migration53 records permanent, proof-free no-forward closures under a request
lock shared by both submission phases and resolution. CP commits closure before
contacting DP. Existing intents use only the persisted original actor/body;
changed body or run conflicts. A canceled or failed DP resolution leaves the
committed closure retryable and prevents delayed forward attempts. DP arbitrates
under its matching admission lock, returning accepted work or its durable
no-effect tombstone. A second CP safety transaction rechecks current configured
installation and read/safety entitlement before making this exact call.

When CP has no intent, its commit-before-send invariant allows local closure.
Observation scope comes from indexed immutable issued grant membership,
including old revisions after rotation/revoke. Remediation requires a known
retained observed run, exact target/slugs and baseline fingerprint. No fresh
user authority is invented: `original_actor_id` is null and the actual
installation `service_actor_id` is retained. No DP request is sent in this case.
The current installation/service and narrow declared safety action still apply;
original-controller Admin membership is not required for resolution.

Normalized grant membership is keyed by org/plugin/grant/revision/project UUID/
Environment UUID. Migration53 rebuilds it exclusively from issued active command
receipts and refuses a current active grant lacking a matching immutable receipt.
Replace validates the entire selection with one SQL join and inserts the new
revision's memberships in one SQL statement, atomically with CAS/history. Revoke
creates no members for the new revision; historical membership remains. Both
TryStart phases select only grant metadata under the same grant fence and perform
an indexed exact membership check. Full target JSON remains for explicit metadata
reads and immutable command receipts, not per-target admission or missing-intent
seal. A 10,000-target real-PG test checks every membership and the actual index
plan; it does not establish a production job-throughput claim.

Focused recovery tests use real PostgreSQL and signed durable HTTP fault peers.
They cover missing intents, historical scope, absent human provenance, baseline
binding, lost seal acknowledgements, accepted/not-started replay, delayed sends,
shared in-flight locks and seal between intent/effect. The peers and controlled
Admin predicate remain component fault tools, not actual combined IAM/DP/Runner
qualification. Actual deployed qualification is tracked separately from these component tests.

Migration53 also guards both existing observation journal tables with BEFORE
INSERT triggers. Older Core processes therefore acquire the same exact request
fence and cannot insert (including ON CONFLICT replay) after any closure state.
An uncommitted old insert prevents local absence classification: after commit,
resolution sees its original routing and contacts DP; after rollback, local
closure is safe. A pending closure similarly blocks old inserts until commit or
rollback. One SQL lock function preserves the existing compact Go JSON key,
including string escaping. It rejects frozen transaction snapshots because the
closure check must observe commits made while waiting; normal Core admission
already explicitly uses READ COMMITTED. Existing accepted receipt/result updates
and GETs remain available. These guards touch only the two observation journals.

## Candidate release impact

This private candidate adds CP migrations52/53 and depends on IAM migration39
for canonical Admin policy. Recovery consumers require both optional Core
protocols and a Data Plane that advertises recovery support; Runner execution
continues to require only the observation execution protocol. Existing manifests
default to no Admin requirement, and omitted Runner limits retain legacy behavior.
Take private backups before upgrade and use tested restore/forward recovery:
guarded downgrades cannot discard retained authority, identities or tombstones.

The generated optional IAM `shared/adminapi` and CP `shared/observationapi`
packages need coordinated additive SDK publication and consumer dependency
updates before standalone published-module builds are reproducible. This
candidate assigns no public SDK or server versions. The local QA workspace
resolves these unpublished packages; CP's canonical workspace-off tidy/security
gates remain blocked by the unavailable IAM admin client and must be rerun after
that dependency is available. A passing private-workspace scan does not replace
those release gates. Runtime, package trust and full recovery qualification are
separate from local source preservation.
