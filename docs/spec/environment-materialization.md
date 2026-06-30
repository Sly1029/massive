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
  -> validate GraphIR + ExecutionContract
  -> resolve symbols
  -> materialize environments for target platform/architecture
  -> upload code package + environment artifacts to datastore
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

- materializes to `.massive/store/envs/<env-key>/`,
- can reuse local package manager caches,
- should still record a manifest and content hashes.

Argo:

- materializes to object storage,
- generated pods fetch or mount the environment artifact,
- container escape hatch may skip package materialization if the image itself is the environment.

Cloudflare/Vercel future:

- may reject unsupported environment specs,
- may translate compatible Node specs into their native bundling/deploy process,
- should still consume the same environment contract.

## Container Escape Hatch

Containers are necessary for Argo and some advanced tools, but they should not be the only environment abstraction.

`env.container(...)` means the author accepts backend portability limits. Other backends may reject the plan or require a backend-specific adapter.

## Open Issues

- Decide how source packages are separated from dependency environments for TypeScript workspaces.
- Decide whether the first Node materializer creates a tarball, an OCI layer, or a backend-specific artifact.
- Decide whether local materialization should require lockfile strictness by default.
- Decide how to expose build logs and cache hits to users.

