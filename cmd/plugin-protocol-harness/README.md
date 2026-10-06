# Local plugin protocol harness

This command is a loopback-only component harness for the plugin
protocol. It loads the real operator installation registry and
`plugins.Gateway`, then exposes only the generic discovery and invoke routes:

- `GET /orgs/{orgId}/plugins`
- `POST /orgs/{orgId}/plugins/{pluginId}/invoke`

It is not the Control Plane server, IAM integration, a production endpoint or
an end-to-end authorization proof. One explicit organization and actor UUID
are accepted through `X-Plugin-Harness-Actor`; the actual plugin still verifies
the gateway's signed, request-bound context and performs its own resource
authorization. Unknown operations fail in the real gateway before reaching a
plugin backend.

The listener must use an explicit loopback IP. Browser access is limited to one
exact configured origin; wildcard CORS is rejected. Installation and PKCS8
Ed25519 signing-key files use the same bounded parsers as the Control Plane.
