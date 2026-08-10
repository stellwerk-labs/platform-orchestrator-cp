# Stellwerk Platform Orchestrator Control Plane

The control-plane service for the Stellwerk Platform Orchestrator. It manages
organization configuration, including projects, environments, module
definitions, rules, and providers.

## Development

Requirements:

- Go at the version declared in `go.mod`
- Docker with BuildKit and Docker Compose
- [yq](https://github.com/mikefarah/yq)
- [score-compose](https://docs.score.dev/docs/score-implementation/score-compose/)
- [golangci-lint](https://golangci-lint.run/)

Go dependencies use public Stellwerk module paths. No GitHub personal access
token or private module configuration is supported. A local Go workspace may be
used while developing coordinated changes across these repositories.

Run the local checks with:

```shell
make install
make generate
make test-unit
go vet ./...
make lint
```

Run the integration suite with:

```shell
make test-integration
make clean
```

The integration environment uses public Stellwerk images from
`ghcr.io/stellwerk-labs` and is generated with Score Compose.

To build only the control-plane image:

```shell
docker build -t platform-orchestrator-cp:local .
```

See `make help` for the complete target list.

## Configuration

Configuration is read from environment variables. See
[`internal/config/config.go`](./internal/config/config.go) for the complete
schema and [`.env.template`](./.env.template) for a local template. Keep real
credentials in an ignored `.env` file or an external secret store.

Control-plane events are published to the NATS JetStream `PO_EVENTS` stream.
Production credentials need publish permission for `io.platform-orchestrator.>`
and do not require stream-administration permission. Stream bootstrap is off by
default. `NATS_BOOTSTRAP_STREAMS=true` is intended only for local development
and creates the control-plane event stream, not runner or dead-letter streams.

## API

See [`openapi/spec.yaml`](./openapi/spec.yaml) for the API definition.
Additional operational endpoints are:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/alive` | Liveness probe |
| `GET` | `/health` | Readiness probe |

## Releases

Pushes to `main` run generation, unit tests, vet, lint, integration tests, and
semantic release. A successful release publishes the multi-platform image as
`ghcr.io/stellwerk-labs/platform-orchestrator-cp:<version>`.

## Compatibility Identifiers

The repository and package ownership is Stellwerk, but the following
`platform-orchestrator` identifiers are intentionally retained because they are
runtime or API contracts rather than GitHub ownership:

- service, workload, executable, environment-variable, and Kubernetes names
  such as `platform-orchestrator-cp`, `platform-orchestrator-dp`, and
  `platform-orchestrator-iam`
- event types in the `io.platform-orchestrator.*` namespace
- persisted Vault paths under `/platform-orchestrator/`
- existing OIDC issuer and discovery settings

Changing these values requires a coordinated compatibility migration across
deployed services and stored data. This OSS ownership migration does not change
them.

## License

Licensed under the [European Union Public Licence 1.2](./LICENSE).
