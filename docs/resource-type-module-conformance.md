# Resource Type conformance and Module output declarations

Status: OSS Core release-candidate contract, implementation qualification pending.
This document does not claim a published API/client release or completed AC13 / AC39.

## Ownership and guarantee

`ResourceType.module_contract` is an optional immutable interface contract.
`ModuleVersion.output_schema` is an immutable author-supplied output declaration.
Neither is Enterprise-owned or dependent on a Rollout entitlement.

Publication validates an Orchestrator-owned JSON definition offline. It does not
download artifacts, inspect Terraform variable/output blocks or run providers.
Declaration conformance is not proof that executable source implements an
interface, and it never changes `verification_status: unverified`.

An external artifact digest is optional. When present, it must be
`sha256:<64 lowercase hexadecimal characters>` and is retained immutably as the
caller's claim. Omission is valid for new external versions and legacy history;
empty/malformed values are invalid, and inline source must omit the field.
Exact source revision remains required for managed external publication.
Neither a revision nor a digest is independent integrity verification. There is
no automatic digest generation, absent-digest backfill or trusted resolver in
this iteration. Adding immutable metadata later requires a new Version.

## Contract document and bounded schema

The contract validates only `module_inputs`, `module_params`, `provider_mapping`,
`dependencies`, `coprovisioned` and an explicitly supplied `output_schema`.
Empty maps/arrays are explicit; parameters expose effective `is_optional` values.
Absent output declarations remain absent. Source, SemVer, lifecycle and release
notes are excluded so an interface contract does not become a release policy.

The root must have `type: object`; root `properties` and `required` may name only
the six fields. No contract means no additional invented constraints.
`{"type":"object"}` is an explicit unconstrained contract; `{}` is not valid.

Supported OpenAPI 3.0 Schema Object keywords: `type`, `properties`, `required`,
`additionalProperties`, `items`, `enum`, `nullable`, `minimum`, `maximum`,
`exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`, `minLength`, `maxLength`,
`pattern`, `minItems`, `maxItems`, `uniqueItems`, `minProperties`, `maxProperties`,
`title`, `description`, `allOf`, `not`. Array schemas require `items`.
`additionalProperties` is boolean or schema; `type` is a scalar type name.
Unknown keywords, `$ref` (even local), remote resolution, defaults, other
composition and executable extensions fail validation. Limits are 64 KiB JSON,
16 schema levels and 64 schema nodes. Do not advertise full JSON Schema support.

Use `allOf` to combine constraints. Required co-provisioned type presence can be
expressed as `not` of an array whose `items` are `not` the required matching
object. Test a matching item, empty array and nonempty unrelated array;
`minItems` alone does not prove the required type is present.

Placeholders are checked as their literal strings, never evaluated during
publication. A contract must explicitly allow that representation if it wants
to permit placeholders. Normal deployment validation resolves concrete values.

## Authoring a declared output

For example, create a Resource Type whose `output_schema` is:

```json
{"type":"object","properties":{"value":{"type":"string"}}}
```

Then publish a complete Version for a catalogue entry permanently bound to it:

```json
{
  "semantic_version": "1.0.0",
  "module_source": "inline",
  "module_source_code": "output \"value\" { value = \"release-one\" }",
  "module_inputs": {},
  "module_params": {},
  "provider_mapping": {},
  "dependencies": {},
  "coprovisioned": [],
  "output_schema": {
    "type": "object",
    "properties": { "value": { "type": "string" } }
  }
}
```

This is an authoring fixture, not evidence of a real deployment. The new
Version starts Proposed/Unverified and requires explicit eligible promotion
before implicit deployment resolution.

Every new managed publication for a nonempty Resource Type output schema needs
an explicit equal declaration, even without `module_contract`. Equality is JSON
value equality: insignificant whitespace and object order do not matter, but
descriptions, constraints and omitted fields do. Schema subsumption is not
implemented. `{}` imposes no output constraint and permits omission, whereas
`{"type":"object"}` is nonempty. Core must not fill a missing declaration by
copying the Resource Type. Historical absence is not an empty schema or proof.

The public request/response definitions are in `openapi/spec.yaml` and generated
clients. History and details retain the declaration; comparisons expose
`output_schema_changed` plus exact typed `before` / `after` declarations.
Provider import/refresh must reconstruct the stored declaration, not substitute
the current Resource Type or discard it as an unknown field. CLI and Console
must allow explicit authoring and display before/after changes.

## Atomicity and diagnostics

Validation occurs inside the publication transaction before the Version,
lifecycle event, outbox event and idempotency receipt exist. Stable graduation
validates the complete successor before deprecating the exact prerelease.
Any failure preserves the prior lifecycle and unused publication namespace.
Internal and public mutation paths enforce identical rules. Provider and
referenced Resource Type existence checks still apply independently.

HTTP 400 uses the normal error envelope with stable codes:

- `resource_type_contract_invalid` for unsupported or malformed contracts;
- `module_resource_type_nonconformant` for definition violations;
- `module_output_declaration_required` for missing constrained declarations;
- `module_output_contract_mismatch` for unequal declarations.

Messages identify the JSON pointer and rule without echoing input values,
source, outputs or credentials. These are not artifact-verification diagnostics.

## Compatibility and operator recovery

This is a deliberate managed-publication write precondition and belongs in the
major authoring/client migration, not a zero-break upgrade claim. Existing data
remains readable; new clients use separate catalogue and immutable Version
resources. Legacy `v0` is not assigned invented SemVer/digest/schema evidence.
New publications and stable graduation must use the explicit declaration; old
authoring endpoints cannot bypass configured contracts.

Resource Types remain immutable and unversioned. A changed contract requires a
new Resource Type and a new Module binding. Archive blocks new bindings, not
publication by existing bound Modules. Permanent Version history keeps its
Resource Type and dependency references; teardown may retain those records.

The schema migration adds nullable contract/declaration columns without changing
old records. Existing reads and exact historical deployments/rollback remain
compatible, subject to normal permissions and source availability. During
application recovery retain additive columns. The explicit Down migration
removes them and is lossless only before declarations exist; otherwise export
the metadata and treat removal as deliberate data loss. It is not routine
application rollback.

Release evidence must cover all six fields, schema bounds/references/regex,
redacted deterministic errors, literal placeholders, absent-vs-empty outputs,
atomic rejection, history/compare/import/refresh, realistic migrations,
legacy deployment and actual CLI/provider/Console authoring. A document or a
passing schema unit test alone does not close these gates.
