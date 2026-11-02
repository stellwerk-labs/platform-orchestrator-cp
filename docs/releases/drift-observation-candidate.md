# Environment observation and Runner capacity, release candidate

This candidate adds the generic interfaces that native plugins need to request
read-only Environment observations. It also adds optional Runner capacity and
protocol declarations. Publication and the coordinated release versions are
still pending.

## Changes

- Add optional `environment-observation/v1` APIs for explicitly authorized
  observations and receipt lookup. The existing shared SDK interfaces remain
  compatible; observation clients use a separate generated package.
- Require canonical organization Admin membership for plugin operations that
  declare the Admin policy. Assigning a granular plugin permission alone does
  not confer that membership.
- Add optional `max_parallel_jobs` and `supported_protocols` Runner settings.
  Existing Runners retain their current behavior when these are omitted.
- Keep corrective deployments on the ordinary deployment path, with current
  authorization, lifecycle, Module Version Pin and baseline checks. Durable
  request identities allow uncertain responses to be reconciled safely.
- Preserve entitlement fencing across real service callbacks without recursively
  acquiring the same authority lock.

## Upgrade

Back up the affected databases and deployment state before upgrading. Use the
qualified coordinated CP, DP, IAM and Runner set; final version and image digest
references will be added after qualification. Publish the additive shared SDKs
before building applications that import their new optional packages.

An observation-capable plugin is installed and licensed separately. This release
does not add plugin business logic to the Control Plane image. Existing plugin
operations retain their declared policies.

Runner capacity is an explicit operator choice. A positive limit bounds concurrent
jobs across Environments that share a Runner. Reducing a limit lets existing work
drain before admitting additional work. Declaring a protocol must match the
Runner executable actually installed.
