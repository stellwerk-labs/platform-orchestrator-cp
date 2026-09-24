# Orchestrator Module command atomicity

Module commands commit their state, lifecycle events, normal outbox records and
idempotency receipt in one PostgreSQL transaction. An unsuccessful command rolls
back all four. Receipts are scoped by organization, operation scope and caller
idempotency key, and bind the complete typed payload fingerprint.

Before looking up a receipt, the transaction takes an advisory lock for that exact
command identity. This also serializes concurrent first submissions, where there
is no receipt row to lock yet. Every replica uses the database lock; a process-local
mutex would not protect multi-replica or restarted services. Hash collisions can
only serialize unrelated requests, not merge their receipts or authorization.

The lock is transaction-owned and is released on commit, rollback or connection
failure. No session-owned lock or manual unlock is used. This follows
[PostgreSQL's transaction-level advisory lock semantics](https://www.postgresql.org/docs/17/explicit-locking.html#ADVISORY-LOCKS).

The command lock does not replace current authorization, expected resource
versions, Module row locks, sorted multi-Module locking, uniqueness constraints or
deployment/Pin eligibility checks. Distinct commands still compete through those
domain constraints. Replaying a receipt must not grant rights that the caller no
longer holds.

Release regression evidence:

- `TestConcurrentModulePublicationReplaysOneImmutableResult` submits eight
  identical concurrent publication commands, checks the same successful Version
  UUID for every response, and verifies one immutable Version and one event.
  Reusing the key for a different definition is rejected.
- `integration-tests/module_lifecycle_atomicity_test.go` checks rejected stable
  graduation and multi-Module lifecycle transactions against exact persisted
  state/event/receipt snapshots, including failure after an earlier write in the
  transaction. Corrected retries and successful command replays are also checked.
- `TestConcurrentStableSuccessorCommandsCreateOneStableProposedVersion` submits
  simultaneous stable-successor commands for the same Proposed prerelease and
  verifies only one command commits, one receipt scope is retained and one stable
  Proposed successor is created.

These are database/API tests against an isolated compatible Orchestrator stack, not
substituted repository mocks. They do not claim production Runner, distribution
or cloud-provider availability testing.
