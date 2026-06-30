# Testing Strategy

Status: draft

Massive should treat tests as functional specifications. Tests must exercise real behavior across the boundaries that matter: TypeScript authoring, IR generation, datastore persistence, environment materialization, backend compilation, and local or Kubernetes execution. Mock functions, spies, monkeypatches, and MagicMock-style replacements are banned.

The rule is mechanical:

```sh
node scripts/check-no-test-mocks.mjs
```

The repository also includes a pre-commit hook in `.githooks/pre-commit`. Enable it with:

```sh
git config core.hooksPath .githooks
```

## Banned Test Doubles

The scanner rejects:

- Vitest mock APIs: `vi.mock`, `vi.fn`, `vi.spyOn`, `vi.stubGlobal`, `vi.stubEnv`.
- Jest mock APIs: `jest.mock`, `jest.fn`, `jest.spyOn`.
- Sinon test doubles.
- Python `unittest.mock`, `MagicMock`, `AsyncMock`, `Mock`, and `patch`.

This does not ban Vitest as a test runner. It bans replacing behavior inside tests. A TypeScript package can still use Vitest for assertions, fixtures, and process-level functional tests.

## Preferred Test Shapes

### SDK Tests

SDK tests should build real workflows with the public TypeScript API, compile them, then inspect the generated `WorkflowPlan` and datastore artifacts.

Useful assertions:

- step symbols are stable,
- GraphIR topology is correct,
- schema references are present,
- default step output artifacts are persisted,
- explicit channel publications have the expected reducers,
- compile diagnostics point to authoring locations.

These tests should not replace graph operations. They should use the real Graphology-backed builder and inspect the lowered IR.

### Datastore Tests

Datastore tests should run against real implementations:

- local filesystem datastore in a temporary directory for fast default coverage,
- S3-compatible object store for protocol coverage.

MinIO is useful for CI and local functional tests because it exercises bucket, key, content type, conditional write, and listing behavior without using a cloud account. The datastore contract should be shared across implementations so the same test suite can run against local filesystem and S3-compatible stores.

### Argo Compiler Tests

Argo compiler tests should have two layers:

- offline bundle generation tests that compile a real `WorkflowPlan` and validate generated YAML against Kubernetes and Argo schemas,
- cluster tests that submit selected bundles to a local Kubernetes cluster and assert terminal status plus expected datastore artifacts.

Your OrbStack or minikube cluster is enough for the cluster layer if it can run the Argo Workflows controller and reach the configured datastore. For early v0 work, a local filesystem datastore is enough for offline compilation tests, but real cluster execution will eventually need either:

- MinIO running in the cluster, or
- an S3-compatible external endpoint reachable from workflow pods.

The cluster test harness should create a namespace per test run, install or verify Argo CRDs/controller, submit the deploy bundle, wait for completion, collect workflow status/logs, and delete the namespace.

### Environment Materialization Tests

Environment tests should materialize real Node environments from lockfiles. Assertions should inspect the manifest, cache key, artifact references, and backend-specific package shape. They should not mock package-manager output. Small fixture packages with committed lockfiles are preferable.

## Local Developer Requirements

For v0, the recommended local stack is:

- Node and the chosen package manager for the TypeScript SDK,
- Deno for the v0 local runner and SDK functional tests,
- `tsgo` for TypeScript validation,
- Go toolchain if backend compilers are written in Go,
- Docker or compatible container runtime,
- OrbStack or minikube Kubernetes cluster,
- Argo Workflows installed in a test namespace,
- MinIO for S3-compatible datastore tests.

The fast path should not require Kubernetes. Most tests should compile plans, validate schemas, and use the local filesystem datastore. Kubernetes tests should be opt-in or separately tagged because they are slower and depend on local cluster health.

The v0 SDK command is:

```sh
deno test --config deno.json --allow-read --allow-write --allow-sys=cpus packages/sdk/test/sdk.test.ts
```

The `--allow-sys=cpus` permission is required by the Node-compatible `fast-glob` dependency.

The local Argo cluster command is:

```sh
pnpm test:argo-cluster
```

That test expects the active Kubernetes context to be `orbstack`, Argo Workflows installed in the `argo` namespace, and the `argo` service account able to create `workflowtaskresults.argoproj.io`.

## Language Split

The current recommended split is:

- TypeScript for the authoring SDK and developer-facing workflow definitions,
- Cap'n Proto as the shared IR boundary,
- Go for backend compilers, datastore tooling, Argo bundle generation, and future target compilers.

This split is reasonable because it keeps the authoring layer close to TypeScript users while putting the portable compiler and backend machinery in a language with strong static binaries, Kubernetes libraries, good concurrency, and straightforward distribution.

The cost is schema discipline. The TypeScript SDK cannot become the real source of truth for semantics if Go owns backend compilation. Shared behavior must live in the Cap'n Proto schema, conformance fixtures, and golden functional tests that both languages consume.
