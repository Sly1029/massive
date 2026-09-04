# Runtime Environment Bindings

Status: design direction, not an implemented provider framework.

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

## Proposed minimal secret contract

The workflow declares a logical name and the environment-variable name its code
expects. The deployment maps that name to a concrete source:

| Requirement | Local/CI realization | Argo realization |
| --- | --- | --- |
| `catalog-api` exposed as `CATALOG_API_TOKEN` | read a configured environment variable | emit `secretKeyRef` for a configured Secret/key |
| object-store access | CI-provided workload identity | pod/workload identity |

This is a design example, not new authoring syntax. The existing contract's
`secrets` name/ref pairs remain the current wire surface; separating logical
requirements from deployment mappings requires a versioned schema change.

Compile should validate binding shape and capability, not require secret values.
Local execution should check required environment variables before starting tasks;
the platform resolves remote secret existence/authorization at deployment or runtime.
Diagnostics name missing requirements, never their values. There is no implicit
`.env` loading and no automatic secret-manager discovery.

Generated requirement inventories are useful for CI setup and review. Prefer deriving
them from the plan rather than maintaining a second handwritten list. An inventory is
not an authorization policy: possession of a name does not grant access.

## Current limits

- Python local processes inherit the launching environment. Declared secrets are not
  yet selectively bound or preflighted.
- Argo rejects declared secrets because secret-reference lowering is not implemented.
- Local execution does not enforce a container, Kubernetes resources, or network policy.
- Argo supports immutable container images, CPU/memory requirements, and limited
  network-policy lowering; it does not implement the proposed mediation/placement model.

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

## Sequence

1. Package source and dependency inputs using standard Python project metadata.
2. Implement dependency preflight and record actual environment realization.
3. Version logical secret requirements and deployment mappings together.
4. Add local env bindings and Argo `secretKeyRef` lowering with functional tests.
5. Add lifecycle events before considering middleware or typed dependency providers.

No first-party vault, provider plugin framework, token proxy, or generic policy renderer
is scheduled.
