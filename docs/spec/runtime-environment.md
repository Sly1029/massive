# Runtime Environment Bindings

Status: selective local bindings and native Argo bindings are implemented.
Provider frameworks remain deferred.

This supersedes the broad facet/provider proposal. Start with explicit requirements
and two concrete deployment modes, not a registry of providers, renderers, proxies,
and middleware.

## Responsibility split

- **Workflow:** declare logical requirements without retrieving credentials at import
  or compilation time.
- **Deployment/CI:** choose concrete bindings, authenticate to a secret store, supply
  short-lived credentials where possible, and own access policy.
- **Massive:** validate required bindings and lower supported ones into task execution.
  Never store secret values in source, specs, descriptors, plans, or bundle manifests.

The same workflow should not need to know whether a credential comes from a CI
secret, a Kubernetes Secret, or a workload identity.

## Minimal secret contract

The workflow declares a logical name and the environment-variable name its code
expects. The deployment maps that name to a concrete source:

| Requirement | Local/CI realization | Argo realization |
| --- | --- | --- |
| `catalog-api` exposed as `CATALOG_API_TOKEN` | read a configured environment variable | emit `secretKeyRef` for a configured Secret/key |
| object-store access | CI-provided workload identity | pod/workload identity |

The existing contract's `secrets` name/ref pairs are the wire surface on both
targets. Bindings remain executor configuration, outside graph identity. Local
`{env}` or `{file}` sources and Argo `{name,key}` sources use the same logical
keys; see [local execution](local-execution.md).

Compile should validate binding shape and capability, not require secret values.
Local execution should check required environment variables before starting tasks;
the platform resolves remote secret existence/authorization at deployment or runtime.
Diagnostics name missing requirements, never their values. There is no implicit
`.env` loading and no automatic secret-manager discovery.

Generated requirement inventories are useful for CI setup and review. Prefer deriving
them from the plan rather than maintaining a second handwritten list. An inventory is
not an authorization policy: possession of a name does not grant access.

## Current limits

- Local task processes receive a documented interpreter base, explicit non-secret
  pass-through, and only declared secrets. Required sources are preflighted.
- Argo binds declared environment secrets through a deployment mapping to native
  Kubernetes Secret keys; see [Argo secret bindings](argo-backend.md#application-secret-bindings).
- Local execution does not enforce a container, Kubernetes resources, or network policy.
- Argo supports immutable container images, CPU/memory requirements, and explicit
  shared-storage egress validation; it does not implement the proposed mediation model.
- Pod placement (node selectors, affinity, tolerations, runtime and priority classes,
  labels, annotations, and a restricted pod spec patch) is deployment configuration;
  see [Argo pod placement](argo-backend.md#pod-placement). Local execution ignores it.

Document these limitations. Do not describe a declaration as enforced merely because
it appears in the plan.

## Security without building a vault

Environment-variable injection is sufficient for trusted workflow code, but it is not
a sandbox: code can print or exfiltrate a credential it can read. Log redaction is
defense in depth, not proof against arbitrary disclosure.

For untrusted code, CI/platform isolation and restricted credentials remain necessary.
Secret mediation proxies, per-host policies, and specialized runtimes can be separate
deployment adapters later. They are not prerequisites for ordinary customer CI, and
Massive should not standardize a sentinel-token or proxy protocol without a real need.

Requirements that demand isolation must eventually fail on incapable targets rather
than silently degrade to ordinary environment inheritance.

## What does not enter dependency identity

Secret values, secret versions, pod placement, namespace, service account, and runtime
network policy are not Python dependency/build inputs. Keep them out of the environment
cache key. Record non-secret deployment bindings separately so operators can audit
what ran where without making credential rotation rebuild dependency environments.

## Deferred work

Source packaging, dependency preflight, selective local environment bindings,
native Argo `secretKeyRef` lowering, and lifecycle events are implemented.
No first-party vault, provider plugin framework, token proxy, or generic policy
renderer is scheduled. Add a new binding source only for a concrete target need.

## Local execution trust

Local workflow authoring and task processes execute trusted application code.
Frontend adapters inherit the invoking environment; task interpreters start with
selective environments. Both may use the local filesystem and network. Loading a graph itself imports author code; local
execution is not a security boundary. The old TypeScript-only Deno permission
flags are removed rather than presented as isolation the Python path or frontend
never provided. Use an isolated execution environment and its native access
controls when running code that must not have host access.

The runner currently performs datastore I/O in the author process. A future local
object-store datastore needs engine-side artifact I/O to keep datastore credentials
out of steps; selective environment bindings alone cannot provide that separation.
