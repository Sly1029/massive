# Environment Materialization

Status: draft

Environments are portable specs first. Container images are an escape hatch and one possible backend realization.

This follows the same broad idea that makes Metaflow's uv support useful: describe the environment at a higher level, then let the backend materialize or validate it.

## Environment Specs

V0 should support:

```ts
env.node({
  version: "22.12.0",
  packageManager: "pnpm",
  lockfile: "pnpm-lock.yaml",
});

env.container({
  image: "ghcr.io/acme/workflow@sha256:...",
});
```

In the portable spec, environments are normalized into an environment table keyed by environment spec hash. Execution contracts reference the environment hash. They do not inline or duplicate the full environment spec.

Future variants:

```ts
env.uv({
  python: "3.13",
  lockfile: "uv.lock",
});

env.nix({
  flake: ".#workflow",
});
```

## Compile Phase

Environment materialization is a distinct compile phase:

```text
source workflow
  -> build graph
  -> SDK validate language-specific symbols and portable lowering
  -> emit WorkflowSpec JSON
  -> Go validate GraphIR + ExecutionContract
  -> materialize environments for target platform/architecture
  -> upload code package + environment artifacts to datastore
  -> write WorkflowPlan
  -> emit backend deployment artifact
```

## Materialization Key

Environment materialization is deduped by unique effective environment spec, not by workflow and not by full step contract.

Resource limits, priority, network policy, and secrets do not change the dependency environment.

A Node environment key should include:

- environment kind,
- Node version,
- package manager and version if pinned,
- lockfile hash,
- package manifest hash,
- workspace package hashes when local workspace packages are included,
- platform OS,
- CPU architecture,
- materializer version,
- relevant build args.

Example:

```text
node@22.12.0 + linux/amd64 + pnpm@9 + lock:<hash> + package:<hash>
```

## Materialized Artifacts

Example Argo materialization output:

```text
s3://bucket/envs/node/<env-key>/manifest.capnp
s3://bucket/envs/node/<env-key>/runtime.tar.zst
```

The manifest describes:

- environment key,
- source hashes,
- platform,
- materializer version,
- runtime entrypoint,
- package manager metadata,
- content digests,
- created artifact paths.

## Backend Behavior

Local async:

- materializes to the configured local datastore, `~/.massive/store/envs/<env-key>/` by default,
- can reuse local package manager caches,
- should still record a manifest and content hashes.

Argo:

- materializes to object storage,
- generated pods fetch or mount the environment artifact,
- container escape hatch may skip package materialization if the image itself is the environment.
- v0 executable support is container-only until Node dependency environment materialization is implemented for Kubernetes.

Cloudflare/Vercel future:

- may reject unsupported environment specs,
- may translate compatible Node specs into their native bundling/deploy process,
- should still consume the same environment contract.

## Container Escape Hatch

Containers are necessary for Argo and some advanced tools, but they should not be the only environment abstraction.

`env.container(...)` means the author accepts backend portability limits. Other backends may reject the plan or require a backend-specific adapter.

Container environments still emit environment manifests. The manifest records the image reference, environment key, materialization mode, and runtime contract. It may explicitly state that dependency materialization was skipped because the container image is the runtime.

Example:

```text
envs/<env-key>/manifest.capnp
  kind: container
  image: ghcr.io/acme/workflow@sha256:...
  materialization: skipped
  runtime:
    sourceFetch: datastore
    stepRunner: image-provided
```

For the first executable Argo path, `env.container(...)` may skip dependency-environment materialization, but it does not skip source package handling. The v0 runtime image contract is:

- the image contains Node and the Massive step-runner CLI,
- the workflow source package is fetched from the datastore at step startup,
- the step runner resolves the symbol from the fetched source package,
- step input and output artifacts are read from and written to the datastore.

This keeps dependency packaging out of the first Argo wedge while still exercising real source packaging, datastore reads, and datastore writes.

## Source Packages Versus Environments

Source packages and environments are intentionally separate.

Source packages answer: which workflow code and local package files are executable?

Environments answer: which runtime, dependency manager, lockfile, platform, and materializer outputs are required to execute that source?

The same source package can be compiled for multiple targets or environments. The same environment can be reused across multiple workflows or source packages when its effective dependency inputs match. Node environment keys may include package manifest and workspace package hashes when those files affect dependency installation, but resource limits, secrets, network intents, and target scheduling settings do not change the dependency environment key.

Workflow packages may have their own `massive.config.ts`, `package.json`, lockfile, utility modules, and config. Those files belong to the source package when they are needed to execute or resolve workflow code. They influence dependency environment materialization only when they affect dependency installation or workspace package resolution.

## Open Issues

- Implement Node dependency environment materialization for deployable targets.
- Decide how source packages are separated from dependency environments for TypeScript workspaces.
- Decide whether the first Node dependency materializer creates a tarball, an OCI layer, or a backend-specific artifact.
- Decide whether local materialization should require lockfile strictness by default.
- Decide how to expose build logs and cache hits to users.
