# Pydantic Graph as an authoring reference for Massive's Python SDK

Research date: 2026-08-18

## Executive recommendation

Use the current Pydantic Graph `GraphBuilder` API as the **authoring-shape
reference** for Massive's Python SDK, but do not use Pydantic Graph as Massive's
portable runtime or serialized graph format.

The useful surface is:

- a typed graph builder with explicit start and end nodes;
- decorated step functions returning typed values;
- an explicit `StepContext` containing input and injected dependencies;
- explicit edges, decisions, maps/broadcasts, joins, and reducers;
- graph validation and rendering before execution.

The parts that do not fit a portable, durable workflow system are just as
important:

- shared mutable graph state;
- arbitrary Python predicates and edge-transform lambdas;
- graph objects that directly retain Python callables;
- in-process `anyio` task execution;
- completion-order reduction without a portable ordering contract;
- implicit discovery of the fork dominated by a join;
- no current persistence or resumability facility.

Massive should therefore deepen its existing compiler boundary: the Python
builder evaluates at compile time and emits the language-neutral
`WorkflowSpec`; production execution always consumes a compiled plan and
resolves stable Python symbols. Pydantic should supply value validation,
serialization, and JSON Schema generation at step boundaries, while Massive's
artifact store and run journal supply durability.

## Which Pydantic Graph API is current?

There are two authoring styles, but the historical labels are now misleading:

1. `BaseNode` is the class-based state-machine style. A node's async `run()`
   method receives `GraphRunContext[state, deps]` and returns an instance of the
   next node or `End`. Its annotated return union is inspected at runtime to
   discover outgoing transitions. This is dynamic, node-directed control flow.
   ([current overview](https://pydantic.dev/docs/ai/graph/graph/),
   [`BaseNode` source at v2.22.0](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/basenode.py))
2. `GraphBuilder` is the functional/declarative style. It registers async step
   functions, and the author separately adds typed edges, decisions, maps,
   broadcasts, and joins. The official documentation describes this API as the
   concise form for parallel dataflow graphs and says it interoperates with
   `BaseNode`. ([builder overview](https://pydantic.dev/docs/ai/graph/builder/))

`GraphBuilder` used to live under `pydantic_graph.beta`, but Pydantic AI 2 moved
it to the top-level `pydantic_graph` package and removed the beta import path.
The same migration removed `pydantic_graph.persistence`, with no V2 graph-state
persistence replacement. ([Pydantic AI V2 changelog](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/docs/changelog.md))

The local workflows lock resolves `pydantic-graph==2.22.0`, so this note uses
the v2.22.0 tagged source rather than older `pydantic_graph.beta` examples. The
builder is no longer imported as beta, but it is still the newer and more
rapidly changing surface. That is a reason to copy its good concepts into an
owned SDK contract, not to make the portable spec depend on its object model.

## Current API and semantics

### Builder and steps

`GraphBuilder` is generic over graph state, dependencies, input, and output. It
creates typed `start_node` and `end_node` handles. `@g.step` wraps an async
function; the node ID defaults to the function name and can be overridden with
`node_id=`. A `StepContext` exposes `state`, `deps`, and `inputs`.
([step documentation](https://pydantic.dev/docs/ai/graph/builder/steps/),
[`StepContext` source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/step.py))

Edges are explicit. The common form is
`g.edge_from(source).to(destination)`, and `g.add(...)` admits one or more edge
paths. The generic types let a Python type checker reject an edge whose source
output does not match its destination input. Pydantic's docs describe this as
generic/static type safety; the graph package itself does not install a
`TypeAdapter` at every edge or otherwise turn annotations into a durable wire
schema. ([type-safety documentation](https://pydantic.dev/docs/ai/graph/builder/steps/#type-safety),
[`GraphBuilder` source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/graph_builder.py))

The builder also offers `stream()` for an async iterator-producing step. This
can feed an async iterable to `.map()`, creating downstream tasks progressively
as values arrive. ([stream and async-map documentation](https://pydantic.dev/docs/ai/graph/builder/parallel/#spreading-asynciterables))

### State and dependencies

`state` and `deps` are graph-run objects handed directly to every step. State is
explicitly mutable and shared by all parallel tasks; the documentation warns
authors to be careful with concurrent mutations. Dependencies are ordinary
injected Python objects passed to `graph.run(deps=...)`.
([step context and dependency injection](https://pydantic.dev/docs/ai/graph/builder/steps/),
[parallel state sharing](https://pydantic.dev/docs/ai/graph/builder/parallel/#state-sharing-in-parallel-execution))

This is convenient in one process, but neither object has portable identity or
serialization semantics. It is especially unsuitable as the source of truth
for retries across Kubernetes pods, Lambdas, or Cloudflare-backed executors.

### Decisions

A decision is an explicit node built with `g.decision()`. Branches come from
`g.match(type_or_literal, matches=...)`; they are checked in declaration order,
and the first matching branch wins. Built-in matching uses `isinstance`,
`Literal` membership, or a catch-all; `matches=` accepts an arbitrary Python
predicate. Branch paths can themselves transform, map, and broadcast values.
([decision documentation](https://pydantic.dev/docs/ai/graph/builder/decisions/),
[`DecisionBranch` source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/decision.py))

This is a good topology form, but an inline predicate is not a portable plan.
For Massive, a portable decision should consume a persisted discriminated value
and contain literal/schema cases. An advanced predicate must be a named
executable symbol with its own environment and execution contract, effectively
a decision step, never a serialized closure.

### Map and broadcast

Pydantic Graph distinguishes:

- broadcast: send one value to several paths; and
- map: split an `Iterable` or `AsyncIterable` into concurrently executing item
  paths.

Both create explicit fork nodes that a later join may synchronize.
([parallel execution documentation](https://pydantic.dev/docs/ai/graph/builder/parallel/),
[`PathBuilder` source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/paths.py))

The current map API exposes two portability warnings:

- An empty iterable only activates its downstream join when the author supplies
  `downstream_join_id`; otherwise no item reaches the join. The docs show this
  special argument explicitly. ([empty iterable documentation](https://pydantic.dev/docs/ai/graph/builder/parallel/#empty-iterables))
- Mapping from multiple source nodes is currently rejected with
  `NotImplementedError`. ([v2.22.0 source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/paths.py))

Massive should preserve explicit fork identity but improve the empty case: an
explicit map-to-join scope in the IR should make the zero-cardinality result an
ordinary empty reduction, not a special sentinel task and not a stringly typed
escape hatch.

### Joins and reducers

`g.join(reducer, initial=... | initial_factory=...)` creates an explicit join
node. Each incoming value is folded into the accumulator. Reducers may be plain
`(current, input) -> current` functions or receive `ReducerContext` with state,
deps, and an operation to cancel sibling tasks. Built-ins include append,
extend, dictionary update, sum, discard, and first-value/race.
([join documentation](https://pydantic.dev/docs/ai/graph/builder/joins/),
[`Join` and reducer source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/join.py))

The runtime associates a join with a *dominating fork*: every path to the join
must pass through that fork. The builder computes the parent fork, with optional
`parent_fork_id` and closest/farthest preference, then the runner tracks fork
instances and reduces values as concurrent tasks finish.
([join behavior](https://pydantic.dev/docs/ai/graph/builder/joins/#how-joins-work),
[`Graph` fork metadata and runner source](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/graph_builder.py))

Two consequences matter for Massive:

1. Reducer arrival order is execution order. The official list examples sort
   results before asserting presentation order, and the source reduces each
   `JoinItem` as it is received. A distributed workflow needs an explicit order
   contract. `collect` should preserve map input index by default; unordered,
   commutative reduction should be an opt-in that enables tree reduction.
2. `edge_from(a, b).to(c)` is not a typed product/gather of `a` and `b`; it is
   multiple source paths delivering values. The builder's examples often use
   shared mutable state to combine heterogeneous branch results. Massive still
   needs a separate, explicit **named gather join** for fixed heterogeneous
   fan-in, alongside Pydantic-style reducer joins for homogeneous dynamic
   fan-out.

Custom reducers in a portable plan must be stable symbols. Built-in reducers
can be declarative IR operations. Any reducer that executes Python, accesses
dependencies, or has effects should run under a normal environment and
execution contract rather than inside a privileged orchestrator process.

### Execution, persistence, and serialization

The built graph runs in-process through `run()`, `run_sync()`, or an async
`iter()` interface. The current runner uses an `anyio` task group for parallel
node work and carries arbitrary Python inputs in `GraphTask` objects.
([execution documentation](https://pydantic.dev/docs/ai/graph/builder/#advanced-execution-control),
[`GraphRun` implementation](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/pydantic_graph/pydantic_graph/graph_builder.py))

The official builder documentation explicitly says there is no native
persistence because consistent snapshots become complex under parallel
execution. Pydantic AI 2 also removed the older persistence package with no V2
equivalent. ([persistence note](https://pydantic.dev/docs/ai/graph/builder/#persistence-and-resumability),
[V2 changelog](https://github.com/pydantic/pydantic-ai/blob/v2.22.0/docs/changelog.md))

There is likewise no language-neutral serialization contract for a built graph:
its nodes and paths retain Python functions for steps, reducers, transforms,
and custom matchers. Mermaid output is a visualization, not an executable
artifact. This conclusion is an inference from the tagged implementation.

For Massive's Python SDK, Pydantic itself—not Pydantic Graph—should provide the durable
value boundary. `TypeAdapter` can validate, serialize, and generate JSON Schema
for arbitrary supported annotations, not only `BaseModel` subclasses.
([Pydantic `TypeAdapter`](https://pydantic.dev/docs/validation/latest/concepts/type_adapter/),
[JSON Schema generation](https://pydantic.dev/docs/validation/latest/concepts/json_schema/))

## Fit for a statically compiled Massive Python SDK

### Copy directly as product vocabulary

| Pydantic Graph idea | Massive Python SDK interpretation |
|---|---|
| `GraphBuilder(input_type, output_type, deps_type)` | Python frontend that builds and validates a workflow before emitting `WorkflowSpec` |
| `@g.step` | Register a stable Python symbol plus typed input/output and compile-time metadata |
| `StepContext.inputs` | Immutable, schema-validated input value or artifact reference |
| `StepContext.deps` | Typed handles resolved from declared dependency providers at task start |
| `start_node`, `end_node`, `edge_from(...).to(...)` | Explicit typed dataflow with readable linear sugar |
| `decision()` and typed/literal `match()` | Explicit decision IR over a persisted discriminant |
| broadcast and map forks | First-class fork nodes with stable fork and instance identities |
| explicit `join(reducer, initial=...)` | Reducer join node with a declared scope and aggregation contract |
| build-time graph validation and rendering | Frontend diagnostics, plan diff, and visualization |

### Deliberately change these semantics

| Pydantic Graph behavior | Massive behavior |
|---|---|
| Mutable shared `state` | No ambient mutable state. Durable values move through typed edges, named channels/artifacts, and explicit joins. |
| Arbitrary in-memory `deps` | Dependency declarations compile to stable provider refs; runtime handles are created per attempt and are never serialized. |
| Python type checker is the data boundary | `TypeAdapter` validates both sides and emits validation- and serialization-mode JSON Schemas; the compiler checks their wire compatibility, and the runner stores canonical JSON or explicit blob/external refs. |
| Function-name node IDs | Convenient default, but compiled IDs and symbol refs are visible in plan diff; explicit `id=` is available for long-lived identity. |
| Inline predicate/transform closures | Literal/schema decisions and declarative projections only; complex behavior is a named step symbol. |
| Shared-state branch convergence | Explicit `gather` for fixed named inputs; explicit reducer join for dynamic homogeneous inputs. |
| Join discovers its parent fork at runtime | Frontend may infer and diagnose, but emitted IR records the exact fork scope. |
| Completion-order list append | Ordered collection preserves source index; unordered reduction declares associativity/commutativity. |
| Empty map requires `downstream_join_id` | Map IR owns its empty-result route to an explicit join handle. |
| Async stream creates in-process tasks | Portable v1 maps a finite persisted collection. Streaming is a later capability-gated node kind. |
| State-machine cycles are expressible | Portable v1 rejects cycles. A later bounded-loop or durable state-machine node is explicit and capability-gated. |
| In-process graph runner | Local, Argo, Lambda, and future backends consume the same compiled plan and task protocol. |
| No persistence | Massive records immutable attempt inputs/outputs and a run journal in its artifact/datastore layer. |

### Proposed authoring shape

This is a directional sketch, not a frozen API:

```python
from pydantic import BaseModel

from massive import Effects, StepContext, WorkflowBuilder, reducers


class ScanRequest(BaseModel):
    repo_url: str


class FileRef(BaseModel):
    artifact: str
    path: str


class Finding(BaseModel):
    path: str
    rule_id: str


class ScanReport(BaseModel):
    findings: list[Finding]


g = WorkflowBuilder(
    name="scan-repository",
    input_type=ScanRequest,
    output_type=ScanReport,
    deps_type=WorkflowDeps,
    environment=base_python_env,
)


@g.step(effects=Effects.IDEMPOTENT)
async def list_files(ctx: StepContext[WorkflowDeps, ScanRequest]) -> list[FileRef]:
    return await ctx.deps.repository.list_files(ctx.inputs.repo_url)


@g.step(environment=native_tools_env, effects=Effects.PURE)
async def scan_file(ctx: StepContext[WorkflowDeps, FileRef]) -> list[Finding]:
    return await ctx.deps.analyzer.scan(ctx.inputs)


collect = g.join(
    reducers.concat(order="input"),
    initial_factory=list[Finding],
    node_id="collect-findings",
)

@g.step
async def report(ctx: StepContext[WorkflowDeps, list[Finding]]) -> ScanReport:
    return ScanReport(findings=ctx.inputs)


g.add(
    g.edge_from(g.start_node).to(list_files),
    g.edge_from(list_files).map(join=collect).to(scan_file),
    g.edge_from(scan_file).to(collect),
    g.edge_from(collect).to(report),
    g.edge_from(report).to(g.end_node),
)

workflow = g.build()
```

The `map(join=collect)` argument is a typed-handle version of Pydantic Graph's
empty-map `downstream_join_id`; it scopes the explicit join but does not create
one automatically.

The step decorator is also the right product surface for environment and effect
metadata. A safe effect model can remain small:

- `Effects.OPAQUE` (recommended default): do not cache, and do not retry after
  an unknown completion outcome;
- `Effects.IDEMPOTENT`: retry is allowed, but caching still requires an explicit
  cache policy;
- `Effects.PURE`: retry and content-addressed reuse are allowed when plan,
  environment, inputs, and declared dependency identities match.

Both `@g.step` and `@g.step(...)` should work, as they do in Pydantic Graph. Sync
and async Python functions should both be accepted even though Pydantic Graph's
steps are async-only; forcing legacy CPU-bound step bodies to become syntactically
async adds migration work without improving the portable contract.

## Required IR consequences

Following this authoring model requires more than extending Massive's current
`mergeInputs` field. The portable graph needs first-class nodes or equivalent
typed records for:

- decision and discriminated cases;
- broadcast fork;
- map fork plus dynamic instance identity and concurrency policy;
- reducer join, including exact fork scope, initial value/factory symbol,
  ordered/unordered semantics, and reducer algebra declarations;
- named gather join for static heterogeneous inputs;
- later, capability-gated race/cancel and stream node kinds.

Every executable function—step, predicate, projection, or custom reducer—needs a
stable source-package and qualified-symbol reference. Every value edge needs a
schema ref. Every dynamic task needs a deterministic logical identity derived
from the fork instance and input position/content, not arrival order.

Persistence belongs below this graph API:

1. compile Python builder objects into a deterministic `WorkflowSpec`;
2. materialize source and environment artifacts;
3. create a `WorkflowPlan` for the selected target;
4. journal task scheduling and attempt state;
5. crystallize each validated output as an immutable artifact;
6. resume only when the plan/environment/input/effect-policy gate permits it.

That preserves the valuable S3 crystallization while making the orchestration
backend replaceable.

## What to validate with the first two manual rewrites

Do not build a corpus migrator yet. Rewrite two workflows by hand and use them
to force the SDK and IR seams:

1. A dynamic map/reduce workflow with a real empty-input case, bounded
   concurrency, ordered collection, and a custom step environment.
2. A branch-heavy workflow with heterogeneous branch outputs, a named gather,
   explicit effects, and at least one injected runtime dependency/secret.

For each workflow, run the exact same compiled-plan fixture through the local
runner and the existing deployment path. Require equivalent canonical output
artifacts, graph visualization, plan diff, environment hash, and retry/resume
decisions. This will expose whether the abstraction is actually backend-neutral
before Argo, Lambda, or Cloudflare adapters multiply.

## Version and design risks

- **Naming drift:** many search results still show `pydantic_graph.beta`; the
  local version and Pydantic AI 2 use top-level imports. Pin source citations and
  avoid mirroring private class names in Massive's IR.
- **No durable contract:** Pydantic Graph intentionally does not solve graph
  persistence under concurrency. Depending on it as a runtime would recreate
  the backend coupling this project is trying to remove.
- **Mutable-state examples hide joins:** some official examples converge
  branches through shared state. They are not a replacement for typed static
  gather semantics.
- **Ordering is underspecified for reproducible artifacts:** reducer arrival
  order is a runtime fact, not a logical map order. Massive must specify this.
- **Closures are pervasive:** transforms, custom matchers, reducers, and steps
  are callable-backed. The Python frontend must reject any executable callable
  that cannot be resolved as a packaged stable symbol, unless it can lower the
  operation to declarative IR.
- **Graph validation is not wire validation:** generic type compatibility helps
  authors, but Pydantic `TypeAdapter` plus canonical serialization must be an
  explicit SDK layer.

The central product decision is therefore: **Pydantic Graph form, Massive
semantics**. That gives users the concise Python graph API they like without
binding durability, environments, retries, or targets to an in-process graph
library.
