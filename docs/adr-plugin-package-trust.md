# ADR: plugin package trust

Status: plugin contract level `v1alpha` for first-party plugins.
Date: 2026-10-08.

## Context

A plugin reaches a cluster as a signed OCI package: a strict plugin manifest,
one static executable per platform and optional UI assets, released together
with a detached signature and signed SBOM and provenance attestations. Two
components handle it:

- The plugin runtime pulls the package, verifies it and runs the executable
  as its own workload.
- Core's Plugin Manager records the package (the SHA-256 of the plugin
  manifest and the registration manifest Core serves for it), activates the
  installation, routes to it and authorizes every effect the plugin asks for.

Signature checks could live in either or in both. This decision says which,
and what Core relies on instead.

## What is checked where

The plugin runtime, in each Pod's init containers and before every start:

| Step | Check |
|---|---|
| `verify-envelope` | The fetched OCI manifest is the package digest the operator pinned and names the plugin manifest hash the operator approved; only layers of the package format are admitted. Nothing else is pulled before this passes. |
| `verify-publisher` | The operator's trust bootstrap (`publishers.json`, never taken from a package) pins the publisher of the plugin ID and its keys. The package signature and the SBOM and provenance attestations are verified offline as one release set against a pinned, unrevoked key; Cosign verifies each part again. |
| `validate` | The plugin manifest, the executable for the platform and the UI assets match their digests; a read-only receipt records the result. |
| `run` | Package and receipt are verified again, the executable is read once, checked, sealed in memory and executed from there, so nothing written to the package volume later changes what runs. The plugin receives `STELLWERK_PACKAGE_SHA256`, the digest of the plugin manifest this verification accepted, and nothing it could inherit instead. |

Core:

| Step | Check |
|---|---|
| Register a package | An operator holding `plugin_manager_write` records the plugin manifest digest with the registration manifest; the same digest never names another manifest. |
| Activate | Core reads the running plugin's status and serves the registrations only while it reports exactly the installation UUID, the package digest and the migration generation Core recorded (`plugin_package_digest_mismatch` and the other diagnostics otherwise). An upgrade switches only when the plugin reports the target digest. |
| Every effect | The effect re-verifies, in its own transaction, that the installation UUID and package digest it was authorized for are still the served ones. Licence grants bind to the installation UUID Core assigned. |

## Decision

Only the plugin runtime verifies package signatures. Core holds no publisher
trust and verifies no package signature. It binds activation, upgrade and
every effect to the package digest that the running plugin reports, which
the runtime derived from its own verification.

Reasons:

- Verification belongs where the bytes run. The runtime checks the package it
  executes, from the bytes it executes; a check in Core would cover a copy
  Core never runs, at another time.
- A second verifier in Core needs a second trust bootstrap (publishers, keys,
  validity windows, revocations) that operators keep in step with the
  runtime's. For first-party plugins it adds no check that the digest binding
  does not already give.
- The runtime verifies offline, so air-gapped installations need nothing from
  Core for package trust.
- Core never executes plugin code. Package trust and licence trust stay
  separate: Core verifies signed licence grants offline with its own issuer
  trust ([plugin-entitlements.md](plugin-entitlements.md)).

## What the binding guarantees

- Core serves a registration manifest only while the running plugin reports
  the digest the operator registered it under. A runtime that started another
  package, or an unverified one, reports another digest and is refused.
- A package registered under a digest never changes; an upgrade is a new
  digest, served only once the runtime reports it.

It does not guarantee:

- That the registration manifest belongs to the package. Core trusts the
  operator holding `plugin_manager_write` to register the registration
  manifest that ships with the package digest.
- Anything about a runtime that lies. The runtime, its init containers and
  its trust bootstrap are part of the trusted base of an installation, like
  Core's own image.
- Verification in development. The runtime's `development-untrusted` policy
  skips signature verification and tells the plugin so; production licence
  trust refuses development material.

## Consequences

- Operators configure publisher trust once, in the plugin runtime chart, and
  rotate or revoke keys there; restarting the runtime applies a change.
- Partner publishers and packages from outside the operator's control need
  more than operator trust for the registration manifest: a registration
  manifest inside the signed package, verified by Core against publisher
  trust, would close that gap. Revisit this decision before the plugin
  contract leaves `v1alpha` for partners.
