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

SDK tests should build real workflows with the public TypeScript API, emit `WorkflowSpec`, then inspect the generated spec and source package artifacts.

Useful assertions:

- step symbols are stable,
- GraphIR topology is correct,
- schema references are present,
- source package manifests contain exact files and hashes,
- steps expose only explicit typed inputs and outputs, not mutable workflow channels,
- emit diagnostics point to authoring locations,
- every supported v0 graph shape emits a valid `WorkflowSpec`.

These tests should not replace graph operations. They should use the real Graphology-backed builder and inspect the lowered spec.

The current v0 graph catalog is the canonical machine-readable contract in [`../../conformance/graph-catalog.json`](../../conformance/graph-catalog.json), with a human-readable view in [`../../conformance/graph-catalog.md`](../../conformance/graph-catalog.md). It covers:

- passthrough `start -> end`,
- single-step workflows,
- linear chains,
- fan-out/fan-in diamond graphs,
- uneven branch-depth fan-in,
- multi-stage fan-in,
- 100-way batch split and merge.

### Datastore Tests

Datastore tests should run against real implementations:

- local filesystem datastore in a temporary directory for fast coverage,
- S3-compatible object store for protocol coverage.

MinIO is useful for CI and local functional tests because it exercises bucket, key, content type, conditional write, and listing behavior without using a cloud account. The datastore contract should be shared across implementations so the same test suite can run against local filesystem and S3-compatible stores.

The default developer datastore is `~/.massive/store`. Tests should use explicit temporary local datastore roots so they do not read or mutate a developer's real local store.

### Argo Compiler Tests

Argo compiler tests should have two layers:

- offline bundle generation tests that compile a real `WorkflowPlan` and validate generated YAML against Kubernetes and Argo schemas,
- cluster tests that install generated `WorkflowTemplate` bundles into a local Kubernetes cluster, submit runs from those templates with the Argo CLI, and assert terminal status plus expected datastore artifacts.

Your OrbStack or minikube cluster is enough for the cluster layer if it can run the Argo Workflows controller and reach the configured datastore. For early v0 work, a local filesystem datastore is enough for offline compilation tests, but real cluster execution will eventually need either:

- MinIO running in the cluster, or
- an S3-compatible external endpoint reachable from workflow pods.

The cluster test harness should create a namespace per test run, install or verify Argo CRDs/controller, apply the generated template bundle, submit a run from the template, wait for completion, collect workflow status/logs, inspect datastore artifacts, and delete the namespace.

### Environment Materialization Tests

Environment tests should use real Python projects and lockfiles. Assertions inspect source identity, materialization inputs, and the installed wheel. Locked dependency realization is still a roadmap item; recording a lockfile does not prove that an execution environment matches it. Do not substitute simulated package-manager output for an installation test.

## Local Developer Requirements

The current local test stack is:

- `uv` and Python 3.12 or newer for the primary SDK and runner,
- Node, pnpm, and Deno for the supported TypeScript frontend and functional tests,
- Go for the compiler, control plane, and fuzzing,
- Protocol Buffers compiler (`protoc`) for schema compile and protojson round-trip conformance checks,
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

The local execution test path should use the same compiled artifact path as production-like targets:

```text
Python or TypeScript SDK emits WorkflowSpec
Go compiler emits WorkflowPlan
Go orchestrator loads WorkflowPlan from datastore
Each step runs in a separate language runner process
```

The retired in-memory TypeScript runner is not an execution path. Tests must exercise the portable plan and invocation protocol.

Run `scripts/test-argo.sh` with Docker, kind, kubectl, Go, and uv available.
The script builds the current locked Python distribution, creates a disposable
kind cluster, installs pinned Argo and MinIO, and runs Go-emitted bundles.
It verifies nested decisions, inactive branches, empty maps, selected-item
failure propagation, and Blob/Tree hydration across separate pods. The cluster
is removed on exit; failed runs preserve resource and pod logs under
`dist/argo-test-logs`. The Argo CI job runs the same command on pull requests.

## Language Split

Python is the primary authoring SDK and runtime; TypeScript remains a supported
frontend. Both emit the same versioned WorkflowSpec and share conformance
fixtures. Protobuf-owned canonical JSON is the portable plan boundary. Go owns
validation, compilation, local orchestration, datastore tooling, and Argo bundle
generation. Neither frontend is an independent source of graph semantics.

## Python graph properties and continuous fuzzing

The current Python-first fast path runs `pnpm check:python`. Hypothesis generates
nested decision trees with uneven branches, checks that emitted specs compile
with the real Go binary, shrinks cross-graph handle failures, and round-trips
Unicode/punctuation directory shapes through the filesystem artifact store.
The regular test suite runs these properties without Kubernetes or a fuzz daemon.
Hypothesis's local example database is ignored by Git; turn a discovered failure
into a named regression before changing the generator.

`./scripts/fuzz.sh` discovers and runs every Go fuzz target. A crashing target
does not stop the campaign; the script reports every failure at the end. The
graph targets share `internal/graphgen`, which decodes fuzz bytes into valid
Graph IR: DAG fan-out and ordered merges, nested decisions and selects, maps
(empty, single, large, under decisions, with `maxConcurrency` below, at, and
above the item count), child graphs expanded into scoped node IDs, and
retry/timeout contracts. Its reference interpreter predicts each node's
activation, skip reason, values, attempts, and map items without sharing code
with the orchestrator.

- `FuzzGeneratedGraphExecution` (`internal/orchestrator`): runs generated graphs
  through `Run` with an in-process executor behind `StepInvoker`. The executor
  reads inputs from the datastore, validates them against plan schemas,
  evaluates the generated symbol, and publishes through the artifact runtime.
  It injects retryable, non-retryable, timeout, orphan-output, and
  missing-output failures, and cancellation or infrastructure failure at a
  chosen batch. The oracle compares the journal with the interpreter and checks
  terminal statuses, skip reasons, attempt bounds and slots, dense map items,
  preserved completed artifacts, producer ordering, and `runjournal.Parse`
  round trips.
- `FuzzGeneratedSpecMutations` (`internal/plan`): breaks one semantic rule of a
  generated spec, recomputes its hash, and requires a semantic diagnostic.
- `FuzzPlanHashMetamorphic` (`internal/plan`): node/edge declaration order,
  aliased schema/contract/environment references, and node/symbol renaming
  must not change the plan beyond the renamed identities.
- `FuzzGeneratedPlanLowering` (`internal/target/argo`): lowers every generated
  plan, including mixed-case, punctuated, and 128-character IDs, and checks
  task names, dependencies, case conditions, merge inputs, map parallelism, and
  retry strategies against the plan; injected name collisions must be rejected.
- `FuzzWorkflowParsing`, `FuzzGraphShapes`, and `FuzzDecisionGraphs` cover raw
  bytes, bare DAG scheduling, and wide decisions; the journal and invocation
  targets cover their parsers.

Set `FUZZ_TIME=5m` and `FUZZ_WORKERS=4` for a longer local campaign, or pass
package paths to fuzz a subset. CI fuzzes each package in its own job: bounded
campaigns on pull requests that touch fuzzed code, longer ones nightly. Only
runs on main (nightly or dispatched) save the corpus cache; pull requests
restore it. Failing inputs are written to the package's
`testdata/fuzz/<target>/` and uploaded by CI. Commit minimized reproducers with
the fix; `go test ./...` replays them as normal tests. Committed scenario seeds
keep rare fault combinations covered without a fuzzing run.

File transport tests use real filesystem stores, independent Python processes,
and MinIO. The wheel installation gate runs the artifact map example using only
the installed distribution. These gates establish protocol and process behavior;
they do not substitute for a live Argo cluster run with workload identity.
