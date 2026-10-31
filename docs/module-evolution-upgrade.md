# Module evolution upgrade qualification

Status: development qualification notes. The feature is not released.

The released starting point is Control Plane v3.1.1 (schema48) and Data Plane
v4.0.1 (schema35). This branch adds CP schemas49/50/51 and DP schemas36/37. Qualify
and record the complete image, SDK, client, Console and chart set before
publishing an upgrade procedure for operators.

## What remains compatible

Published definitions, SemVer reservations, lifecycle history and existing
Default pointers are not rewritten. Historical global and organization-specific
Resource Types receive their exact old interface as version1, each with its own
immutable UUID. Unknown historical actors remain absent. Old callers that omit
`resource_type_version` continue to select1, even after later interfaces exist.

Existing provider-owned Module definitions retain their configured JSON.
Imports must preserve a selected later interface. Changing an interface cannot
force replacement of an unrelated Module or silently adopt a newer interface.
An authoring Draft lives outside published history and cannot be selected as an
executable Version merely because its UUID or intended SemVer is known.

## Upgrade order and recovery

Take a recoverable PostgreSQL backup and record exact deployed image digests,
chart values, schema versions and outstanding work before applying migrations.
Test the restore procedure outside the live database. Retain queued, in-flight
and rollback snapshots during qualification.

Upgrade the compatible DP before enabling the new CP workflows. It must read
old catalogue responses and old execution records without inventing metadata.
Then upgrade CP and finally the Console. CLI/provider publication must use the
qualified generated contracts, with public-source checks passing against the
recorded server commit. No image may depend on an ignored local Go workspace.

Do not use a blind Helm rollback to the old binaries. Older CP/DP startup checks
refuse schema versions they do not support. After new metadata exists, schema
Down guards also refuse to erase it. In particular, an old DP could otherwise
discard unknown JSON fields while serializing a retained graph or plan; schema36
exists to prevent that loss as well as validate the stored contract shape.

Recover application behavior with schema-compatible images or a forward fix.
A database restore is a separate operation with an explicit recovery point and
potential loss of later work, not a normal lifecycle rollback. Draft discard,
Module archive and Terraform destroy never erase retained authoring or
published history. A full demo factory reset remains a separate destructive
operator procedure.

## Required rehearsal

Seed schema48/35 with published and legacy Modules, same-name global/custom
types, an active Pin, queued and in-flight work, and rollback material. Upgrade
DP first, then CP, restart both and verify identity and exact source/schema
retention. Test the schema-compatible recovery image set too.

Run existing deployments and plugin work alongside new interface selection.
Then verify editable authoring, immutable test execution, stored-revision
publication and every client/Console workflow. A CRUD test or a local workspace
build alone does not qualify the coordinated release. Record commands, source
commits, immutable artifacts, outcomes and remaining gaps in the traceability
matrix before any public release action.
