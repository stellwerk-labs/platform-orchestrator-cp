# Development-only offline entitlement issuer

`cmd/plugin-entitlement-dev` is a standalone development tool. It is not part of
the Core server startup path or a production issuer. A fresh invocation creates
an Ed25519 key and issuer, a strict development trust file, and one signed
COSE_Sign1 grant bundle. No network or existing key is used for fresh issuance.
How Core evaluates the result is described in
[plugin-entitlements.md](plugin-entitlements.md).

Use the exact plugin ID, immutable installation UUID, Core organization UUID,
and sorted feature IDs from the verified installed package. For example:

```sh
go run ./cmd/plugin-entitlement-dev \
  -out-dir /absolute/new/private/development-issuer \
  -plugin-id dev.example.plugin \
  -installation-id <canonical-installation-uuid> \
  -organization-id <canonical-core-organization-uuid> \
  -features feature-a,feature-b \
  -valid-for 24h -grace-for 24h
```

The output directory must be an absolute **new** directory under a real
non-symlink parent. The tool creates it as `0700`; files are exclusive-create
`0600`. It refuses overwrite and symlink parents and removes its own partial
directory on write failure. It prints only a status and output path.

| File | Use |
| --- | --- |
| `issuer-private-development.pem` | Disposable PKCS#8 Ed25519 private key. Never mount into Core or a plugin. |
| `issuer-development.json` | Private marker binding this development issuer, key and original installation. Not licence authority. |
| `trust-development.json` | Public pinned trust for explicit Core development mode only. |
| `bundle-development.json` | One exact signed grant, no revocation snapshot. |

Validity and grace are each bounded to seven days, with one-second resolution.
The grant starts at issuance, has sequence `1`, and binds exactly one plugin,
installation, organization, and sorted feature set. It contains a signed
`trust_class: development` claim. The tool verifies its own trust file and
signed bundle with Core's strict parsers before writing them. Production mode
rejects the same signed bytes even if their public key is copied into a
production-labelled trust file.

For a development-only renewal, keep the original issuer directory private and
point to the latest signed bundle. The binding and feature flags must be exactly
the same as the first issuance:

```sh
go run ./cmd/plugin-entitlement-dev \
  -out-dir /absolute/new/private/renewal-2 \
  -issuer-dir /absolute/original/private/development-issuer \
  -previous-bundle /absolute/current/private/bundle-development.json \
  -plugin-id dev.example.plugin \
  -installation-id <same-canonical-installation-uuid> \
  -organization-id <same-canonical-core-organization-uuid> \
  -features feature-a,feature-b \
  -valid-for 24h -grace-for 24h
```

Renewal checks the original private key against the pinned development public
key and issuer marker, verifies the prior signed bundle, requires a later
expiry, then creates a new issuance UUID at the next positive sequence. The
new private directory contains **only** `bundle-development.json`, so the key
is not copied. For another renewal, keep `-issuer-dir` unchanged and point
`-previous-bundle` at the latest renewal bundle. A stale branch or equal
sequence with different signed bytes is rejected by Core's monotonic import;
do not deploy an older bundle over the current one. The tool does not query or
change Core's database.

These files are disposable protocol fixtures for development and test
installations. Do not copy, rename, or promote them into production authority.
This command does not install an entitlement, configure a cluster, start a
deployment, or establish commercial licensing. The operator separately places only the
public trust and bundle into Core's development-mode local file configuration.
