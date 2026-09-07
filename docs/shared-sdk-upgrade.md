# Module Management Go client upgrade

The generated Control Plane SDK for Module Management uses
`github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2`.
The existing `shared/v1.0.0` tag and its import path remain unchanged.

This is a source-breaking SDK update, separate from the Control Plane image's
version. In particular, Module history now returns `CoreModuleVersionPage`
containing complete `CoreModuleVersionDetail` records. A version detail exposes
its immutable definition together with its lifecycle, canonical UUID, migration
generation and optimistic-concurrency revision. Retaining the old SDK's major
version would break downstream builds on an ordinary dependency upgrade.

```go
import cp "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v2/genclient"

// response.JSON200.Items contains CoreModuleVersionDetail values.
// item.Version identifies lifecycle state and the command resource revision.
// item.Definition contains the immutable Orchestrator-owned definition.
```

Update imports of `shared/genclient`, `shared/genevents` and `shared/errcodes`
to the corresponding `shared/v2/...` paths when adopting this SDK. Update
Module history consumers to the new response types and migrate authoring to
catalogue creation, immutable publication and explicit promotion. An import-path
change alone does not migrate an old mutable authoring workflow.

Consumers that only use unchanged API/event contracts may remain on the old SDK;
they must not assume its authoring requests create a new Default on the new server.
The release's client/server compatibility matrix defines supported workflows.

## Publication order

The shared SDK is a nested Go module. Its release tag must therefore have the
form `shared/v2.0.0-rc.N` for a candidate, or `shared/v2.0.0` for the first stable
release. A root application tag does not publish this nested module. Publish the
reviewed source and SDK tag before upgrading a consumer to download that version.
Never advertise an uncreated candidate tag as already available.

The parent Control Plane's local `replace ... => ./shared` is intentional, so its
own build and tests use the exact generated SDK from the same source revision.
Other repositories must use an explicit candidate dependency or isolated local
build override during staging, not overwrite a published module cache entry.

This follows Go's [major-version update contract](https://go.dev/doc/modules/major-version).
