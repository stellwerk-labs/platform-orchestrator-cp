# Control Plane security maintenance

Unreleased. These notes describe the local maintenance branch, not an available
artifact or a completed deployment.

## Security changes

Official builds use Go 1.26.10, including the certificate verification fix for
[CVE-2026-111943](https://pkg.go.dev/vuln/GO-2026-6809).

Existing HTTP, cryptography, compression, OpenAPI and telemetry dependencies are updated to patched versions. Reference descriptions now use valid OpenAPI 3.0 composition, without changing JSON fields or permissions. Generated server middleware preserves authentication, including rejection of missing and invalid identities.

Normal CI now checks source dependencies with pinned govulncheck 1.9.0 and fails
on reachable known vulnerabilities. A passing source scan is not a guarantee
against unknown vulnerabilities or a substitute for deployment qualification.

Docker context filtering also excludes `.envrc` variants and direnv state. A
network-disabled Docker COPY test checks these exclusions with public sentinel
files and verifies that application source remains in the context.

## Compatibility and release checks

The SDK retains its published authorization and Pin action constant names,
including `Prepare` and `Begin`, as typed aliases of the generator's explicitly
prefixed names. The former `UserIdHeaderScopes` constant remains available too.
Existing Go consumers continue to compile, and the wire values do not change.
The new prefixed names are preferred for new code; generated files remain
managed by the normal generator.

Existing published versions and artifacts remain immutable. Use a new patch
version after the normal integration and artifact checks pass. Source builds
following the supported release tooling require Go 1.26.10. Clients using the
corrected vendored API documents must verify their exact published server source
pins before release.
