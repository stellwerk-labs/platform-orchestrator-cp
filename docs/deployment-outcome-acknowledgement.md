# Acknowledging an unknown Deployment outcome

A failed plugin-dispatched Deployment whose Runner outcome is unknown blocks its
Environment until either a genuine late result arrives or an operator
acknowledges that the attempt can no longer change the Environment. The Data
Plane decides and keeps the authoritative record; its runner dispatch recovery
documentation describes when an acknowledgement is accepted. The Control Plane
offers the public, Environment-scoped action:

```text
POST /orgs/{orgId}/projects/{projectId}/envs/{envId}/deployments/{deploymentId}/actions/acknowledge-outcome
{"reason": "Runner job deleted after node loss; no pod remains"}
```

- The acting user needs `deployment.outcome.acknowledge` on the Environment,
  with the organization as fallback. Core checks it, then the Data Plane checks
  it again for the same user. Core does not grant this permission; IAM roles
  must include it explicitly.
- The system user and the deprecated organization token are rejected with 403,
  because the actor is the audit record.
- The reason is trimmed and must contain 1 to 1000 characters.
- The Deployment must belong to the addressed project and Environment;
  otherwise the response is 404 and nothing is forwarded.
- Core forwards the actor and reason to the Data Plane and returns its result:
  201 for a new acknowledgement, 200 with `replayed: true` for an identical
  repeat. Data Plane 400, 403, 404 and 409 responses pass through with their
  error code and message, for example when the Runner dispatch window has not
  closed or a different acknowledgement already exists.

The Data Plane stores Deployment, Environment, plugin, operation, actor, reason
and time. Core keeps no second copy and logs only the identifiers of a newly
recorded acknowledgement, not the reason.

The API gateway must route this exact path to the Control Plane. Environment
sub-paths are forwarded individually, so the route needs its own gateway rule.
