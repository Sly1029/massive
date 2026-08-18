# Replacing Metaflow With a Portable Workflow Compiler

Status: research note
Date: 2026-08-18

> This note records primary-source findings and options considered during
> discovery. Compatibility-layer recommendations here were not accepted. See
> [Workflow Platform v2 Direction](../spec/workflow-platform-v2.md) for the
> normative decisions. Corpus-specific follow-up belongs in a workflows
> repository worktree under `~/worktrees`.

This note uses primary sources only: official documentation, specifications, and source repositories. Factual observations are separated from recommendations and inferences.

## Executive conclusion

Do not start a second workflow project and do not try to turn Massive into another all-owning orchestrator. Make Massive the shared compiler, artifact protocol, and execution-contract kernel, then add a native Python frontend and Python runtime adapter beside the existing TypeScript frontend.

The product should have two independently selectable axes:

1. **Orchestrator**: local scheduler, Argo, Cloudflare Workflows, Lambda durable execution, or eventually Temporal.
2. **Step executor**: local subprocess, Kubernetes container, ordinary Lambda function, Cloudflare Container, Worker isolate, or a target-specific remote service.

This separation matters for any native Linux workload. A Cloudflare Worker is a plausible low-latency coordinator, but it is not a general Linux process host. Native tools belong in a Cloudflare Container, Kubernetes container, or Lambda-compatible native package and can be invoked by an edge workflow. Massive should compile this split deliberately instead of pretending every target can run every step.

The accepted transition is a clean native Python rewrite. A compatibility package
was considered below and rejected because it would preserve the implicit state
and serialization constraints the new model is intended to remove.

## Sourced facts

### What Metaflow actually provides

Metaflow's useful contract is larger than a DAG syntax:

- It statically infers a directed graph from `FlowSpec` step methods and `self.next(...)`. Static source parsing is intentional so the graph can be translated to statically defined runtimes. A step is the smallest resumable unit and a checkpoint at which output state is snapshotted. Instance attributes such as `self.x` become persisted artifacts, while stack variables do not. ([Metaflow technical overview](https://docs.metaflow.org/internals/technical-overview))
- Its public transition surface covers linear edges, static fan-out, dynamic `foreach`, and conditional switches. Join steps receive incoming flow objects, and `merge_artifacts` handles non-conflicting attributes. ([FlowSpec API](https://docs.metaflow.org/api/flowspec))
- Step decorators attach compute, resources, retry, timeout, secrets, and environment behavior. The same decorators may be injected at run time with `--with`, so execution semantics do not necessarily appear in the flow file. ([Step decorators](https://docs.metaflow.org/api/step-decorators))
- Custom decorators can run before, after, on failure, or instead of user step logic and may mutate artifacts. Mutators can alter decorators, configs, and parameters, but currently cannot add or remove graph steps. ([Custom decorators](https://docs.metaflow.org/metaflow/composing-flows/custom-decorators), [composition overview](https://docs.metaflow.org/metaflow/composing-flows/introduction))
- Metaflow persists immutable snapshots of code, data, and external dependencies and records run metadata. Its datastore is content-addressed for code and data, although deduplication is limited across different flows; the metadata provider makes run results discoverable. ([Metaflow technical overview](https://docs.metaflow.org/internals/technical-overview))
- The Client API exposes a `Flow -> Run -> Step -> Task -> DataArtifact` hierarchy and preserves origin pathspecs for resumed results. ([Client API](https://docs.metaflow.org/api/client))
- Resume creates a new run that reuses successful tasks from an origin run and runs unsuccessful or missing work. Metaflow's runtime source describes cloning task metadata and prefetched artifacts into the resumed run. ([Runner/resume documentation](https://docs.metaflow.org/metaflow/managing-flows/runner), [runtime source](https://github.com/Netflix/metaflow/blob/master/metaflow/runtime.py))
- Metaflow maps its DAG to an Argo `WorkflowTemplate`, snapshots the working-directory code and Metaflow version at deployment, maps parameters, and runs every task on Kubernetes. ([Metaflow Argo documentation](https://docs.metaflow.org/production/scheduling-metaflow-flows/scheduling-with-argo-workflows))
- In `uv` mode Metaflow ships the uv project for remote tasks, but does not receive the stronger package snapshotting used by its PyPI/Conda environments. ([Metaflow uv documentation](https://docs.metaflow.org/scaling/dependencies/uv))
- Metaflow labels its documented API as backward-compatible. Its internal graph, runtime, datastore, and plugin implementation files are described in the internals documentation but are not part of that public API list. ([API stability statement](https://docs.metaflow.org/api), [technical overview](https://docs.metaflow.org/internals/technical-overview))
- Metaflow's own extensions template warns that the extension mechanism relies on internal APIs that are not guaranteed to be stable across versions. ([Metaflow extensions template](https://github.com/Netflix/metaflow-extensions-template))

**Migration implication (inference):** translating only `@step` and `self.next` will undercount the compatibility surface. The corpus must also be inventoried for `self.*` artifact semantics, joins, foreach, parameters/configs, runtime-injected decorators, custom decorators, Client API reads, resume assumptions, and ambient Metaflow globals such as `current`.

### Portable IR and backend boundaries

Argo validates the current static-DAG choice but should remain a lowering target, not Massive's IR:

- Argo is a Kubernetes CRD and models steps as containers. Its DAG templates name dependencies explicitly, support nested DAG/steps templates, conditional dependency expressions, and fail-fast behavior. ([Argo repository](https://github.com/argoproj/argo-workflows), [DAG documentation](https://argo-workflows.readthedocs.io/en/latest/walk-through/dag/))
- `WorkflowTemplate` is a cluster-resident reusable workflow definition. ([WorkflowTemplate documentation](https://argo-workflows.readthedocs.io/en/latest/workflow-templates/))
- Artifact repositories are configured independently and can be selected per workflow via `artifactRepositoryRef`, which is useful for keeping credentials and duplicated storage configuration out of templates. ([Artifact repository reference](https://argo-workflows.readthedocs.io/en/latest/artifact-repository-ref/))
- Argo memoization requires a caller-supplied key and stores cache records in ConfigMaps. Its own docs say memoization is designed for pure steps but cannot enforce purity, and document the ConfigMap 1 MiB size failure mode. ([Argo memoization](https://argo-workflows.readthedocs.io/en/latest/memoization/))

Temporal illustrates the boundary Massive should defer rather than absorb:

- Temporal workflows are durable executions recovered by replaying event history. Workflow code must generate the same command sequence on replay; external operations belong in Activities. ([Temporal workflow definition](https://docs.temporal.io/workflow-definition), [workflow execution](https://docs.temporal.io/workflow-execution))
- Worker failures are hidden from workflow code, and a workflow can last from seconds to years. ([Temporal workflow definition](https://docs.temporal.io/workflow-definition), [workflow execution](https://docs.temporal.io/workflow-execution))

**Design implication (inference):** a future Temporal target should use a small, versioned, deterministic plan interpreter that schedules generic Massive activities by symbol and artifact reference. Massive should not translate arbitrary user step functions into Temporal workflow code or duplicate Temporal's event-history service.

Other projects contribute useful product concepts, not a wholesale replacement:

- Prefect work pools are typed infrastructure templates between orchestration and execution; platform teams can expose governed job variables while workers provision infrastructure. ([Prefect work pools](https://docs.prefect.io/v3/concepts/work-pools), [deployments](https://docs.prefect.io/v3/concepts/deployments))
- Hamilton derives a DAG from ordinary function parameters and separates the function graph from adapters/executors. Its dynamic execution groups nodes into tasks and delegates those tasks to local and remote executors. ([Hamilton concepts](https://hamilton.apache.org/concepts/), [parallel execution](https://hamilton.apache.org/concepts/parallel-task/))
- Parsl separates a dependency-aware DataFlowKernel from executors, providers, and launchers, allowing the same Python app to run locally or on remote systems. ([Parsl quickstart](https://parsl.readthedocs.io/en/latest/quickstart.html), [plugin model](https://parsl.readthedocs.io/en/latest/userguide/advanced/plugins.html))
- Pydra explicitly combines Python/shell tasks, selectable compute platforms, map/reduce splitting, global caching, and per-task software environments. ([Pydra documentation](https://pydra.readthedocs.io/))
- Flyte's first-generation `flyteidl` repository describes itself as the protobuf IR specification for tasks and workflows, while its current SDK centers a composable `TaskEnvironment` with an image, retry, and cache behavior. ([FlyteIDL repository listing](https://github.com/flyteorg/flyteidl), [current Flyte SDK](https://github.com/flyteorg/flyte-sdk))
- Dagster's core product framing is declarative assets plus lineage and observability. ([Dagster documentation](https://docs.dagster.io/))

### Environment and artifact primitives

- OCI images already provide content-addressable, platform-specific configuration and ordered filesystem layers. An image manifest references config and layers by digest, while an image index can select platform-specific manifests. ([OCI manifest specification](https://github.com/opencontainers/image-spec/blob/main/manifest.md), [OCI image config](https://github.com/opencontainers/image-spec/blob/main/config.md))
- BuildKit lowers build frontends to a protobuf Low-Level Build (LLB) dependency graph that is concurrently executable, cacheable, and frontend-neutral. It supports registry/local cache import and export and OCI output. ([BuildKit README](https://github.com/moby/buildkit#exploring-llb))
- `uv.lock` carries exact resolved versions; `uv run --locked` rejects a stale lockfile, and exact sync removes undeclared packages. uv workspaces share one lockfile. ([uv locking and syncing](https://docs.astral.sh/uv/concepts/projects/sync/), [uv workspaces](https://docs.astral.sh/uv/concepts/projects/workspaces/))
- `uv.lock` is uv-specific, while PEP 751 standardizes the tool-independent `pylock.toml` dependency-lock format and uv can export it. ([uv project layout](https://docs.astral.sh/uv/concepts/projects/layout/), [PEP 751](https://peps.python.org/pep-0751/))
- Python has standard, static dependency metadata in `[project]` via PEP 621 and inline single-file script metadata via PEP 723. ([PEP 621](https://peps.python.org/pep-0621/), [PEP 723](https://peps.python.org/pep-0723/))
- PEP 735 standardizes named dependency groups outside published package metadata and allows one group to include another, providing a portable primitive for composing development or task-specific dependency sets. ([PEP 735](https://peps.python.org/pep-0735/))
- Nix store objects can be content-addressed, and derivation outputs are determined by their declared inputs, but Nix is a separate expression/build ecosystem rather than a Python environment format. ([Nix content addressing](https://nix.dev/manual/nix/2.28/store/store-object/content-address), [Nix reproducibility tutorial](https://nix.dev/tutorials/first-steps/towards-reproducibility-pinning-nixpkgs.html))
- S3 provides strong read-after-write consistency for object writes and list operations, but an S3 `ETag` is not always an MD5 digest, including for some multipart and encrypted uploads. S3 supports explicit object checksums. ([S3 consistency model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel), [S3 object integrity](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html))

**Design implication (inference):** Massive should own a normalized environment contract and provenance, but delegate actual OCI construction and build caching to BuildKit. `uv` should be the first Python materializer. Nix should remain a later expert escape hatch, not a required layer in the common path.

### Lambda and Cloudflare are restricted targets, not small containers

Ordinary Lambda:

- A normal invocation is capped at 900 seconds and 10,240 MB memory. A container image can be 10 GB uncompressed; it must target one Linux architecture, run on a read-only root filesystem, and use `/tmp` for 512 MB to 10,240 MB of writable storage. ([Lambda quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html), [Lambda container requirements](https://docs.aws.amazon.com/lambda/latest/dg/images-create.html))
- Lambda container images must implement the Lambda Runtime API; AWS, OS-only, and non-AWS base images are supported when the runtime interface client is present. ([Lambda container requirements](https://docs.aws.amazon.com/lambda/latest/dg/images-create.html))

Lambda durable execution is a distinct orchestration target:

- Durable functions replay a checkpoint log across multiple Lambda invocations, skipping completed steps and substituting persisted results. Code outside durable steps must be deterministic. ([Lambda durable concepts](https://docs.aws.amazon.com/lambda/latest/dg/durable-basic-concepts.html))
- The durable SDK exposes step, wait, callback, invoke, parallel, and map operations for JavaScript/TypeScript, Python, and Java. ([Lambda durable SDK](https://docs.aws.amazon.com/lambda/latest/dg/durable-execution-sdk.html))
- A durable execution can last up to one year, while each individual invocation retains the ordinary 15-minute cap. Current quotas include 3,000 durable operations and 100 MB of cumulative persisted payload per execution. ([Lambda durable concepts](https://docs.aws.amazon.com/lambda/latest/dg/durable-basic-concepts.html), [Lambda quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html))

Cloudflare Workers:

- Paid Workers have 128 MB memory, a 10 MB compressed script limit, one-second startup, and up to five minutes of active CPU per HTTP request; waiting on network I/O does not count as CPU. ([Workers limits](https://developers.cloudflare.com/workers/platform/limits/))
- Workers provide web APIs and a subset of Node APIs. `node:child_process` is explicitly a compatibility **stub**, so a native subprocess cannot be treated as a supported Worker execution mode. ([Workers compatibility flags](https://developers.cloudflare.com/workers/configuration/compatibility-flags/#enable-nodechild_process-module), [Workers runtime APIs](https://developers.cloudflare.com/workers/runtime-apis/))

Cloudflare Workflows is a distinct durable orchestration target:

- Workflows provides persisted, retryable steps, waits, events, lifecycle control, and automatic retries. The engine may replay code outside `step.do`, so side effects and nondeterminism must be isolated in steps. ([Cloudflare Workflows overview](https://developers.cloudflare.com/workflows/), [rules](https://developers.cloudflare.com/workflows/build/rules-of-workflows/))
- The Python SDK supports declarative DAGs using function parameter names as dependencies. ([Cloudflare Python DAG workflows](https://developers.cloudflare.com/workflows/python/dag/))
- On the paid plan each step defaults to 30 seconds and can be configured to five minutes active CPU, a non-stream result is limited to 1 MiB, persisted instance state defaults to 1 GB maximum, and a workflow defaults to 10,000 steps. Cloudflare advises storing large artifacts in R2 and returning a reference. ([Cloudflare Workflows limits](https://developers.cloudflare.com/workflows/reference/limits/))

Cloudflare Containers is a distinct full-Linux execution target:

- Containers run existing container images with a full filesystem and support arbitrary languages, runtimes, native binaries, CPU parallelism, and larger memory/disk workloads. A Worker acts as the control plane that starts and communicates with container instances. ([Cloudflare Containers overview](https://developers.cloudflare.com/containers/))
- Current instance types range from 256 MiB to 12 GiB memory; custom types can provide up to 4 vCPU, 12 GiB memory, and 20 GB disk. ([Cloudflare Containers limits](https://developers.cloudflare.com/containers/platform-details/limits/))

**Native workload implication (inference):** “run on Cloudflare” should not imply “run inside a Worker isolate.” A Worker or Cloudflare Workflow can coordinate and deliver results while dispatching a native task to a Cloudflare Container. Lambda and Kubernetes remain alternative native executors selected by policy and availability.

## Assessment of Massive as it exists

The current design already contains most of the correct seams:

- [`overview.md`](../spec/overview.md) defines a language-neutral `WorkflowSpec`, a compiled `WorkflowPlan`, target compilers, and external language runners.
- [`ir-and-datastore.md`](../spec/ir-and-datastore.md) separates source packages from environments, makes target incompatibility a compile-time diagnostic, defines a step invocation descriptor, and uses an object-store-first content-addressed layout.
- [`environment-materialization.md`](../spec/environment-materialization.md) deduplicates effective environments independently of resources/secrets/network policy and leaves container images as a realization rather than the only authoring abstraction.
- [`argo-backend.md`](../spec/argo-backend.md) treats Argo as a generated deploy bundle and validates invariants after customization.

The main architectural correction is that the current `target` concept conflates orchestration and step execution. Argo happens to provide both a DAG controller and Kubernetes pods, but Cloudflare Workflows calling Lambda, a local orchestrator dispatching Kubernetes jobs, or Temporal scheduling mixed workers do not fit that assumption.

The main product correction is prioritization. The specification currently says that no Python frontend is scheduled. For replacing a corpus of Metaflow workflows, the Python frontend and migration analyzer should move ahead of broad TypeScript authoring features, plugin/patch richness, and a hosted metadata plane.

## Recommended product surface

### 1. Keep one compiler kernel

Massive should own five deep contracts:

```text
Python / TypeScript authoring
            |
            v
     portable WorkflowSpec
       graph + effects + schemas
       environment requirements
       orchestration requirements
            |
            v
     capability validation
            |
   +--------+---------+
   |                  |
orchestrator        step executor
lowering            lowering
   |                  |
   +--------+---------+
            v
   immutable deploy bundle
   + content-addressed artifacts
```

The five contracts are:

1. **Frontend contract**: SDKs emit the same `WorkflowSpec` and stable symbol references.
2. **Portable graph contract**: DAG, branch, map/foreach, fan-in/join, schemas, retry intent, and declared effects.
3. **Execution contract**: environment, resources, secrets, network, storage, observability, and required runtime capabilities.
4. **Runner protocol**: a versioned step invocation plus artifact references, heartbeats/cancellation, logs/events, and an outcome receipt.
5. **Artifact protocol**: immutable blobs, manifests, run references, cache records, retention roots, and provenance.

Do not put Python reflection, Metaflow decorators, Kubernetes fields, Lambda handlers, Worker bindings, or Temporal replay rules into the portable core. Those belong in frontends and target plugins.

### 2. Split orchestration from execution in the IR

Replace a single target request with two joined requests:

```text
orchestrator:
  kind: local | argo | cloudflare-workflows | lambda-durable | temporal

defaultExecutor:
  kind: local-process | kubernetes-container | lambda | cloudflare-container | worker-isolate

node.executorOverride: optional executor reference
```

A capability profile should be machine-readable and versioned. Compile-time negotiation must check at least:

- graph: branch, parallel, dynamic map, nested map, join/reducer;
- durability: sleep, signal/callback, resume, cancellation, max operations/history;
- runtime: language, native process, writable filesystem, network model, architecture;
- artifacts: maximum inline value, streaming, external object reference;
- operations: timeout, retry, heartbeat, idempotency, side-effect class;
- environment realization: process environment, OCI image, Lambda image/zip, bundled isolate/Wasm.

Targets should reject unsupported nodes with an explanation such as:

```text
scan requires native_process and 2GiB memory
cloudflare-worker provides native_process=false and memory=128MiB
suggested executor: cloudflare-container, lambda-container, or kubernetes-container
```

### 3. Make effects and cache semantics explicit

> **Decision update:** the accepted UX exposes cache substitution and ambiguous
> rerun guarantees directly rather than leading with the effects taxonomy
> explored in this section.

Content-addressed storage is not equivalent to execution memoization. Add an effect/cache policy to each node:

- `effects: pure | idempotent | external-side-effect`;
- `cache: disabled | run-local | project | global`;
- `cacheKeyInputs`: symbol digest, source digest, environment realization digest, declared input artifact digests, selected configuration, and optional user salt;
- `retrySafety`: safe, requires idempotency key, or forbidden;
- `idempotencyKey`: a stable run/node/attempt-independent operation key for external APIs.

Only `pure` nodes should receive global automatic reuse. Idempotent nodes may be retryable but should not be silently skipped unless the author opts in. External side effects should default to no memoization and require a stated retry policy.

Use three storage layers:

```text
blobs/sha256/<digest>              immutable bytes
manifests/<kind>/<digest>.json     typed Merkle-style metadata
projects/<project>/runs/<run>/...  mutable/logical references and receipts
```

An `ArtifactRef` should contain digest, size, media type, schema digest, optional encoding/compression, and storage locations. A task outcome receipt should atomically bind the exact input set and execution identity to output refs. Retention/GC walks live manifests and run roots; it must not infer liveness from object prefixes.

Use the protocol's explicit digest as the identity and verify it on upload/download. Do not treat an object-store `ETag` as a portable content digest.

Do not depend on Argo's ConfigMap memoization for Massive correctness or cross-target caching. Emit Argo-compatible fields only as a backend optimization when the Massive cache contract says it is safe.

### 4. Treat environment materialization as a target-aware build graph

Normalize composable author intent into a semantic environment before materialization:

```text
runtime        python 3.13 / node 24 / wasm
system         apt packages or base image constraints
language deps  uv.lock / pylock.toml / pnpm lock
dependency sets PEP 735 groups and explicit composition
local deps     workspace/package manifests and content hashes
runner         Massive runner protocol version
build policy   indexes, secrets, network, reproducibility mode
platform       target OS/arch/ABI and target runtime
```

Then produce one or more target realizations:

| Target | Preferred realization |
|---|---|
| local subprocess | cached venv/runtime directory plus manifest |
| Argo/Kubernetes | OCI image digest, built with BuildKit |
| Lambda | Lambda-compatible single-arch OCI image or zip manifest |
| Cloudflare Container | OCI image digest plus instance-type and lifecycle policy |
| Cloudflare Worker | bundled Worker module/Wasm plus bindings manifest |
| Temporal activity worker | OCI image or managed local environment |

The environment key must include normalized dependency inputs, target platform/ABI, runner protocol, materializer version, relevant build policy, and any build-time secrets by stable identity (never secret value). It must exclude CPU/memory, runtime secrets, retry policy, and unrelated scheduling fields.

Implement `env.uv` first for Python, accepting `uv.lock`, PEP 735 dependency-group composition, and PEP 723 single-file metadata; optionally export/store PEP 751 `pylock.toml` for tool-neutral provenance. Use BuildKit for OCI output and cache transport. Add Nix only when a corpus workflow demonstrably needs non-container system reproducibility that uv plus BuildKit cannot express.

### 5. Add a Python frontend without coupling it to Metaflow

The native API should prefer ordinary typed functions and an explicit graph builder, matching the current TypeScript model:

```python
flow = workflow("scan_repo", input=Repo, output=Report)

@flow.step(environment=python_env, effects="pure")
def checkout(repo: Repo) -> Checkout: ...

@flow.step(environment=native_tools_env, executor="container")
def scan(checkout: Checkout) -> Findings: ...

flow.connect(flow.start, checkout, scan, flow.end)
```

Function parameters and return annotations are useful inference, as Hamilton demonstrates, but explicit edges must remain available for branch/map/join and migration. The compiled IR references module plus qualified symbol; it never serializes Python closures or pickles functions as the portable contract.

Use `pyproject.toml` under `[tool.massive]` for package configuration and PEP 621/uv data where it already exists. Allow PEP 723 metadata for one-file local workflows.

## Rejected compatibility migration option

The phases in this section are retained as the alternative that was evaluated,
not as an implementation plan. The accepted approach is a native per-workflow
rewrite with migration automation considered only after two manual rewrites.

### Phase A: inventory before designing compatibility

Build `massive migrate audit <paths...>` as a read-only corpus analyzer. Report counts and exact locations for:

- graph forms: linear, split/join, foreach, condition, recursion;
- parameters, configs, includes, schedules/triggers;
- all decorators, including those injected in wrapper commands;
- custom decorators and extension imports;
- `self.*` writes/reads/deletes and join `inputs` usage;
- Client API, `current`, `S3`, cards, checkpoint, resume assumptions;
- dynamic source constructs the analyzer cannot prove;
- environment sources and target-specific configuration.

This report should decide the compatibility wedge. Do not guess from a few representative flows.

### Phase B: preserve step bodies with a bounded adapter

Create a separate `massive-metaflow-compat` Python distribution. A codemod changes imports, ideally without changing method bodies:

```python
from massive.compat.metaflow import FlowSpec, step, Parameter
```

The adapter statically lowers supported `self.next(...)` forms into `WorkflowSpec`. At execution, a compatibility runner:

1. hydrates a flow-state envelope from named artifact refs;
2. instantiates a lightweight flow object;
3. invokes the original step method;
4. snapshots changed `self.*` fields to a typed/opaque state envelope;
5. maps join `inputs` to read-only branch-state proxies;
6. emits ordinary Massive output refs and a task receipt.

This is intentionally less elegant than the native SDK. Its value is reducing the initial rewrite surface and preserving user Python libraries and step code. Unsupported decorators or dynamic graph behavior must fail the migration audit; they must not silently disappear.

Keep this adapter out of the core schemas. The portable node is still a normal symbol plus input/output schemas; only the Python runtime understands the legacy state envelope.

### Phase C: provide result-reading continuity

Implement a small compatibility facade for the subset of the Client API used by the corpus. Map a Massive run manifest to Flow/Run/Step/Task/DataArtifact-like views, including an `origin` link for reused outputs. Preserve stable logical IDs, but do not reproduce Metaflow's internal storage layout.

For old historical data, start read-only: retain Metaflow as an archive and allow references/import by pathspec. Copy data into Massive CAS lazily only when a workflow explicitly adopts it; record the origin pathspec and original content hash.

### Phase D: shadow and incrementally rewrite

For each workflow class:

1. compile with the compatibility frontend and inspect the plan;
2. run locally against frozen inputs;
3. compare artifact schemas/digests or semantic comparators;
4. run on Argo in shadow mode;
5. switch scheduling while retaining rollback;
6. rewrite high-change steps into native functions when it pays off.

Migration success should be measured by percent of workflows that compile, percent of runs matching outputs, median changed lines, local iteration latency, environment cache hit rate, and number of target-specific hacks removed.

### What not to promise

Do not promise perfect compatibility for arbitrary Metaflow extensions, implicit pickled Python object graphs, cards/UI behavior, or every resume/client query. Publish a compatibility matrix and make unsupported features visible in the audit before users attempt a run.

## Target sequence

1. **Local process + Python frontend**: prove migration and preserve the full compile/plan/runner path.
2. **Argo + container executor**: prove source/environment materialization, object-store artifacts, retry, and operational bundles.
3. **Cloudflare Container executor**: reuse the OCI runner path for native workloads and place a small Worker in front as coordinator. This proves that an executor can be selected independently from orchestration.
4. **Ordinary Lambda executor**: useful for bounded event-driven steps; enforce 15-minute, architecture, filesystem, memory, and packaging constraints.
5. **Cloudflare Workflows orchestrator + Worker executor**: support web-native steps and externalize large results to R2/S3. Allow nodes to select Cloudflare Container, Lambda, or Kubernetes executors for native work.
6. **Lambda durable orchestrator**: add when AWS-only deployment simplicity is valuable and its operation/history quotas fit the graph.
7. **Temporal orchestrator**: add only when signals, long waits, loops, very long histories, or cross-service durable coordination create demand that Argo/Cloudflare/Lambda durable cannot meet.

The first five steps validate the differentiator—portable plans, environments, artifacts, and target explanations—without committing to a new always-on control plane.

## Product risks and decisions to force early

1. **Compatibility budget:** which Metaflow features must work unchanged, and which workflows may require a rewrite? “All existing flows” changes the project from a frontend migration into a Metaflow reimplementation.
2. **Python priority:** for a Python-heavy adoption corpus, delaying Python while expanding the TypeScript SDK optimizes the wrong surface.
3. **State model:** are arbitrary Python objects a requirement, or may the new native SDK require JSON/Arrow/file artifacts with explicit codecs? Cross-language and edge portability depend on this answer.
4. **Execution topology:** does “inside Cloudflare” permit a Cloudflare Container, or must a native process execute in a Worker isolate? The latter conflicts with the Worker process and memory model. If Cloudflare Containers are unacceptable operationally, may a Worker orchestrate Lambda/Kubernetes work instead?
5. **Durability semantics:** is step-boundary retry/resume enough, or are signals, long sleeps, callbacks, and cyclic state machines near-term requirements? Only the latter justifies an early Temporal/Lambda-durable-level contract.
6. **Cache trust:** may a pure-step cache be shared across projects/users, and who can attest that code/environment/input digests and declared effects are truthful?
7. **Control plane:** is an object-store run index adequate initially, or must users query/tag/compare thousands of historical runs with low latency? The latter requires a metadata index sooner, but it should consume manifests rather than become execution truth.
8. **Name/product scope:** is Massive an internal compiler for one workflow repository, or a general platform product? Build the migration wedge first; branding and public generality should follow demonstrated portability.

## Historical compatibility go/no-go milestone

This milestone applied to the rejected compatibility option. The accepted v2
gate is defined in
[Workflow Platform v2 Direction](../spec/workflow-platform-v2.md#graph-ir-v1-acceptance-gate).

Before expanding Massive's target/plugin surface, require one vertical result:

- corpus audit covers every Metaflow file;
- compatibility frontend compiles the dominant graph/decorator subset;
- ten representative flows run locally without changing their step bodies;
- three representative flows run on Argo with uv-lock-derived environments;
- artifacts can be inspected and a failed run can reuse successful predecessors;
- `massive explain --orchestrator cloudflare-workflows --executor worker-isolate` rejects a native-process step and identifies `cloudflare-container` (or another permitted native executor) as eligible.

If this works, building the replacement into Massive is strongly justified. If it fails because the corpus depends pervasively on arbitrary `FlowSpec` object mutation or undocumented extensions, keep Massive as the new platform but migrate workflow-by-workflow through explicit native Python graphs rather than deepening the compatibility layer.
