# Workflow Platform v2 Direction

Status: accepted product direction; Graph IR remains unstable (`0.x`)
Date: 2026-08-18

This document records the decisions reached while evaluating how to replace
Metaflow for Python users. It is the normative direction for Massive's Python
SDK and the next version of Massive. The API examples are directional,
not a frozen public API.

The supporting research is in:

- [Replacing Metaflow by deepening Massive](../research/metaflow-replacement.md)
- [Pydantic Graph as an authoring reference](../research/pydantic-graph-v2-sdk.md)

The [roadmap](../roadmap.md) narrows the next milestone to Python packaging and
reliable local/CI execution. The environment and runtime-binding designs replace
the broad materializer/provider ambitions below with standard Python project
metadata and explicit deployment responsibilities. Future graph syntax here is
not a current SDK promise.

## Decision

Replace Metaflow rather than emulate it.

Massive will provide a native Python authoring SDK that emits its
language-neutral graph IR. Existing workflows will be deliberately rewritten;
there will be no Metaflow compatibility frontend, compatibility runtime, or
legacy pickle codec. This is an opportunity to remove accidental state,
channels, decorator coupling, and target-specific behavior instead of carrying
those constraints into a new system.

Deepen Massive rather than create a second workflow core. Massive should own:

- a versioned, language-neutral graph IR;
- graph validation and target compilation;
- a portable invocation and artifact protocol;
- content-addressed data crystallization;
- composable environment construction;
- target capability validation and backend adapters.

Massive should not become a hosted scheduler or a general metadata service.
Durable systems such as Argo should schedule compiled plans. Later targets can
include Cloudflare, Lambda, and other orchestrators or executors
without changing workflow semantics.

Python is the priority authoring language for v2. TypeScript remains supported
and can converge on the same authoring features later. Both frontends must emit
the same IR and pass the same conformance fixtures.

## Product promise

The author should be able to express a typed workflow once, run the same
compiled plan locally and on Argo, and move it to another capable target without
rewriting its business logic. Inputs and outputs should survive process and
machine boundaries without the author manually uploading, downloading, or
threading S3 paths through step code.

The system optimizes for:

1. explicit and inspectable graphs;
2. immutable typed data between steps;
3. deterministic compilation and provenance;
4. durable retry and resume behavior;
5. reusable environments and execution contracts;
6. honest target portability through capability checks.

It does not initially optimize for source compatibility with Metaflow,
transparent import of historical runs, arbitrary Python object serialization,
or a bespoke durable scheduler.

## System boundaries

```text
downstream domain SDKs
  domain tools, models, and friendly behavior wrappers
                       |
            Massive Python SDK       Massive TypeScript SDK
              typed GraphBuilder       existing/evolving frontend
                       \                    /
                        +- versioned Graph IR -+
                             |
                 validation + compilation
                             |
          WorkflowPlan + invocation descriptors
                    /                    \
          Artifact Runtime        Environment builder
          FS / S3 / R2 CAS          uv / BuildKit / OCI
                    \                    /
                     target adapters
                local | Argo | future targets
                             |
                   target-owned scheduler
                             |
                    language step runner
```

The important seams are:

- **Frontend versus IR:** Python and TypeScript authoring APIs are adapters. No
  Python callable, Pydantic object, or Graphology object belongs in the IR.
- **Orchestrator versus executor:** a target chooses how graph work is scheduled
  separately from where a task process runs. A Cloudflare Worker may coordinate
  a native process running in a Cloudflare Container.
- **Workflow versus deployment:** `WorkflowSpec` describes semantic behavior.
  `DeploymentSpec` selects profiles, credential/secret bindings, namespaces, and targets
  without mutating that behavior.
- **Runner versus artifact storage:** the runner consumes artifact handles. A
  deep Artifact Runtime hides whether bytes live on a filesystem, S3, or R2.
- **Generic infrastructure versus domain SDK:** Massive has no application-specific
  concepts. Downstream SDKs retain their domain models, integrations, policies,
  and higher-level behavior wrappers.

Repository ownership follows that seam. Massive contains the generic Python
builder/emitter and runner alongside its TypeScript frontend, schemas, compiler,
artifact runtime, and target adapters. Downstream repositories contain domain
wrappers, migration reports, and application workflows. A domain SDK may
re-export generic Massive authoring types to present one coherent Python import
surface.

## Domain vocabulary

| Term | Meaning |
| --- | --- |
| Graph IR | Versioned, language-neutral semantic graph emitted by a frontend. |
| WorkflowSpec | Canonical serialized graph plus schemas and semantic contracts. |
| WorkflowPlan | Target-independent executable plan produced from a validated spec. |
| DeploymentSpec | Target/profile bindings such as namespace, credential/secret bindings, and capacity. Raw credentials never serialize. |
| Execution contract | Reusable environment, resources, secrets, network, storage, and capability requirements for a task. |
| Invocation descriptor | Exact runner input: symbols, artifact references, schemas, destinations, and attempt identity. |
| Artifact manifest | Typed description of crystallized outputs whose bodies have already been committed. |
| Map scope | A finite dynamic subgraph instantiated once per crystallized collection element. |
| Reducer | A durable task that folds the ordered results of a homogeneous map scope. |
| Gather | Static, heterogeneous assembly of named upstream values into a typed model. |
| Outcome | Internal `Success[T]` or structured `Failure` produced by every task attempt. |
| Target capability | A qualitative or quantitative property required by a plan, such as native execution, duration, scratch space, payload size, or fan-out. |

## Python authoring form

Adopt the current Pydantic Graph `GraphBuilder` form, not its runtime or IR.
Useful properties are typed step handles, explicit edges, a context object, and
first-class decisions, maps, broadcasts, and joins. Massive must own persistence,
durability, artifact handling, and compilation because Pydantic Graph does not
provide those contracts.

The following is a forward-looking sketch of the intended reducer tier. It is
not executable against the current SDK: finite `map()` returning an ordered
`list[R]` is implemented, while dependency injection, reducer contexts, and
`reduce()` remain future work.

```python
from pydantic import BaseModel
from massive import GraphBuilder, ReducerContext, StepContext, execution


class Item(BaseModel):
    key: str


class Finding(BaseModel):
    item: Item
    count: int


scan = execution(
    environment="native-tools",
    resources={"cpu": 2, "memory": "4Gi"},
    timeout="20m",
)

g = GraphBuilder(deps_type=Services)


@g.step(contract=scan)
async def enumerate_items(ctx: StepContext[Services, Request]) -> list[Item]:
    return await ctx.deps.catalog.items(ctx.inputs)


@g.step(contract=scan)
def inspect(ctx: StepContext[Services, Item]) -> Finding:
    return run_inspection(ctx.inputs)


@g.reducer(initial=0)
def total(ctx: ReducerContext[Services, int, Finding]) -> int:
    return ctx.accumulator + ctx.item.count


items = g.add(enumerate_items)
findings = g.map(items, inspect, concurrency=20)
result = g.reduce(findings, total)
g.add(g.edge_from(result).to(g.end))
```

The reducer syntax may change after prototypes and real workflow rewrites. The
following properties are not optional:

- topology is explicit and inspectable before execution;
- step inputs and outputs are statically typed and validated at runtime;
- data flows through handles, not mutable workflow attributes;
- sync and async Python steps are both accepted; the runner detects and awaits
  async results;
- reusable graph factories receive an explicit ID prefix so node identity is
  stable;
- module-qualified symbols and schemas are serializable and reproducible;
- dynamically created or otherwise unstable nodes are rejected at emission.

`StepContext.inputs` carries workflow data. `StepContext.deps` contains typed
service or capability handles only. Dependencies must not become an invisible
second dataflow channel. Their deployment binding and cache participation are
explicit.

The decorator should remain small. A step references a reusable execution
contract and may set the few semantic behaviors that belong to the node. CPU,
memory, secrets, network, timeout, retry limits, environment, executor, and
capabilities belong in the contract, with workflow defaults and step overlays.

## Graph semantics

### Immutable explicit dataflow

V2 has no shared mutable workflow state and no global channels. A step may only
consume declared inputs and declared dependency handles. Every value crossing a
step boundary is validated, serialized, and crystallized.

This keeps graph analysis, caching, replay, and multi-target compilation local
and understandable. Published values intentionally shared across runs belong in
a separate artifact catalog with explicit publish/read operations; they are not
workflow state.

### Node vocabulary

The initial IR needs only the semantic vocabulary demonstrated by real
workflows:

- start and end;
- task;
- exhaustive decision;
- broadcast fork;
- finite map scope;
- heterogeneous gather;
- durable reducer join.

Loops, streaming maps, wait-any, race, cancellation, compensation, and similar
features should be added only when a rewritten workflow demonstrates the need
and a target can state the required capability honestly.

Decisions switch on a `Literal` or discriminated-union tag and must be
exhaustive. Arbitrary predicates and transformations remain ordinary typed
steps; the IR does not serialize Python callables.

### Map scopes and lineage

A map is a first-class scope, even though the SDK offers concise `g.map(...)`
sugar. Graph IR 0.3 deliberately starts with one mapper task and one
value-producing map node: it expands a finite, already-crystallized `list[T]`,
runs the mapper under bounded concurrency, and publishes a source-ordered
`list[R]`. Its body may eventually contain multiple tasks without changing the
structured execution-scope identity used by item artifacts.

The compiler tracks scope lineage:

- a value from an ancestor scope may broadcast into a descendant map;
- values from the same lineage may compose;
- unrelated sibling lineages require an explicit `zip` or `cross` operation;
- cardinality mismatches are rejected rather than guessed.

This model is more precise than inferring rank from nested lists and prevents
accidental Cartesian products.

An empty Graph IR 0.3 map publishes canonical `[]` at its ordinary output slot.
It does not need a sentinel item or a string ID naming a downstream join. When
explicit reducers land, an empty collection will reach the reducer's declared
initial value.

### Gather and reduce are distinct

`g.gather(Model, field=handle, ...)` assembles a fixed set of heterogeneous
ancestor outputs into a typed model. The compiler verifies dominance and scope
lineage; it is not limited to immediately adjacent nodes and assumes nothing
about a repository layout.

A reducer consumes homogeneous outputs from a dynamic map scope. Graph IR 0.3
first exposes the ordered collected list as an ordinary value; an author may
feed it to a normal typed step today. First-class durable reducer nodes are the
next semantic slice. Collection order is source-index order, never
task-completion order. Built-in associative reducers can be optimized later
without changing this contract.

### Failure and retry have tiers

Do not encode every friendly behavior as a new graph primitive.

1. The runner always records `TaskOutcome[T] = Success[T] | Failure[StepFailure]`.
2. The target applies permitted attempt retries before exposing an outcome.
3. The Graph IR has two generic operations: require success (the default) or
   capture the typed outcome.
4. A downstream SDK can lower a friendly wrapper such as
   `on_error=errors.capture()` to the generic capture operation.

An uncaptured failure fails its graph scope. A captured failure is ordinary
typed data, including inside a map, so a downstream reducer can inspect both
successes and failures. `StepFailure` is a safe structured model; tracebacks and
large diagnostics belong in telemetry or diagnostic artifacts.

There is no initial `emit_error`, `route_to`, compensation, or map failure-policy
vocabulary. Add a semantic primitive only after a real use case cannot be
expressed cleanly as an SDK wrapper over the small IR.

### Versioning

Every Graph IR artifact declares a version. During design it remains `0.x` and
breaking changes are allowed. Consumers and backends declare supported version
ranges and reject unsupported inputs explicitly.

Do not freeze Graph IR v1 until two representative rewritten workflows pass the
acceptance gate below. The invocation, artifact, and runner protocol can
stabilize earlier because it is already the strongest seam in Massive.

## Step behavior and cache identity

Most authors should not need to understand an abstract pure/idempotent effects
ladder. Expose the two decisions that affect execution directly:

- **Cache substitution:** may a prior result be used instead of running this
  invocation? The safe default is off; an opt-in mode keys by execution identity.
- **Ambiguous rerun:** may user code run again after the system cannot prove
  whether a previous attempt completed? Values should distinguish safe rerun,
  rerun requiring an idempotency key, and unsafe rerun.

A plain `@g.step` therefore does not reuse cached results. Infrastructure may
retry before user code starts, but it does not silently rerun after ambiguous
completion. The runtime supplies a stable `ctx.idempotency_key` for every retry
of one logical invocation.

Operational attempt limits and backoff remain part of the execution contract.
Friendly presets can be added later if the manual rewrites reveal combinations
that authors repeat and understand.

For the first cache implementation, use one execution identity hash. It includes
semantic inputs only:

- implementation symbol and code/package identity;
- environment artifact identity;
- input and output schemas;
- input artifact hashes;
- semantic dependency binding or version tokens.

Scheduling choices such as CPU, memory, timeout, retry count, namespace, and
target do not change a result identity. They remain visible in the compiled plan,
whose complete content is independently hashed for provenance. Do not introduce
multiple contract-hash taxonomies until a concrete invalidation problem requires
them.

Every dependency declares whether it is excluded from caching, contributes a
binding identity, or supplies a semantic version token. A mutable dependency
that supplies neither makes the step ineligible for cache substitution.

## Artifact Runtime and S3 crystallization

Massive already has the right skeleton: invocation descriptors reference input
artifacts and output destinations; datastore adapters support local storage and
S3; runners validate and hash values; Argo steps exchange references rather than
large parameters. V2 should deepen that into an Artifact Runtime rather than
replace it.

Step code normally sees typed values or artifact handles, never S3 URIs. The
initial value kinds are:

- canonical Pydantic/JSON values;
- `Blob` for opaque files;
- immutable `Tree` for directory-shaped data;
- explicit external references when ownership stays outside Massive.

`Blob` and `Tree` inputs are handle-based from the beginning, with operations
such as `open()`, `path()`, or an isolated `workdir()`. The first implementation
may eagerly hydrate behind the handle. That preserves room for lazy download,
range access, and bounded local scratch without changing step signatures.

A hydrated tree is an isolated writable working copy. Its mutations disappear
unless the step returns it as a declared new `Tree` output.

Artifact bodies are content-addressed within a security realm. A run-scoped
output manifest points to those bodies. Commit order is:

1. serialize, hash, upload, and verify all bodies;
2. atomically publish the output manifest;
3. journal attempt success.

The manifest location is deterministic for an attempt, allowing recovery if a
runner crashes between manifest publication and success journaling. Cache scope
starts at project-within-realm, never as an unqualified global bucket.

Retention needs reachability or leases plus object-store metadata. Lifecycle
tags alone are insufficient because content-addressed objects are shared and
self-copying an object merely to reset age is expensive and fragile.

The Tree Merkle format still needs an implementation decision covering path
ordering, executable modes, symlinks, empty directories, and excluded metadata
such as modification time.

Argo initially uses pod workload identity. Credentials are deployment bindings,
not IR fields. Presigned or brokered access can be added later without changing
artifact semantics.

## Composable environments

Environment construction follows a `Recipe -> Plan -> Artifact` pipeline:

- **Recipe:** author intent, composed from workflow defaults and step overlays;
- **Plan:** fully resolved and canonical build inputs;
- **Artifact:** immutable environment identity, normally an OCI image.

Start with `uv` for Python resolution and BuildKit for OCI production. The
environment cache key contains only semantic build inputs: target platform,
lock data, base image digest, build files, toolchain versions, and declared
environment variables. Runtime secrets and scheduling choices never enter it.

The execution contract is the single policy surface for environment, resources,
secrets, network, storage, observability, and capabilities. A step references a
named or inline contract instead of accumulating independent decorators.

## Targets and portability

Argo is the first cloud deployment target. Local and Argo must consume the same
compiled plan and runner protocol. Local execution is not a separate in-process
semantic shortcut.

Targets are modeled as an orchestrator plus per-node executor capabilities.
Capabilities include quantitative limits, not only booleans: maximum duration,
payload, scratch space, fan-out, concurrency, and native-process availability.
Compilation fails with a useful explanation when a plan cannot run faithfully.

This keeps these future paths open:

- a Cloudflare Worker coordinating native work in a Cloudflare Container;
- Lambda for steps that fit its execution and storage limits;
- Temporal or another durable orchestrator consuming the same runner protocol;
- mixed plans where different node classes select different executors.

A Worker isolate is not treated as a native-process executor. Target marketing
names must not conceal runtime constraints.

`WorkflowSpec` identity is independent of `DeploymentSpec`. A Kubernetes
namespace or temporary `v2-` prefix is deployment configuration and can be
settled during rollout without entering the Python authoring model.

## Migration boundary

Migration reviews established that dynamic maps, joins, parameters, retry/catch
behavior, environment tooling, and artifact inputs are important enough to
shape the generic platform. Corpus-specific reports, rewrite notes, fixtures,
and migrated applications belong in their source repositories, not in Massive.

This public repository contains only generic platform contracts and synthetic
examples.

Migration is a clean per-workflow cutover:

1. manually rewrite one dynamic map/reduce workflow;
2. manually rewrite one branch-heavy workflow;
3. run them in a separate Kubernetes namespace or deployment prefix;
4. cut each workflow over when its outputs and operational behavior are accepted;
5. build migration automation last, from repeated mechanical edits actually
   observed in those rewrites.

There is no shadow-compatibility runtime, dual execution requirement, or eager
historical artifact import. Old runs remain in their old namespace; new runs
start under v2.

## Delivery sequence

1. Reconcile the docs around this direction and keep Graph IR explicitly `0.x`.
2. Complete a read-only corpus shape audit in the workflows worktree before
   freezing graph semantics; bring only generic conclusions back to Massive.
3. Stabilize and generalize the artifact, invocation, and runner protocol.
4. Add the Python builder/emitter and Python runner with sync/async support.
5. Add Pydantic `TypeAdapter` validation, canonical serialization, and generated
   validation/serialization JSON Schemas at every boundary.
6. Implement the small graph IR: decisions, map scopes, gather, reducer, and
   captured outcomes.
7. Rewrite the two representative workflows by hand.
8. Run the full compiled path locally through the real artifact runtime.
9. Build composable `uv`/BuildKit environments.
10. Compile and execute the same plans on Argo with S3 or MinIO artifacts.
11. Pass the v1 acceptance gate, then stabilize Graph IR v1.
12. Add Cloudflare, Lambda, or other targets based on real demand.
13. Add a migration assistant only after the manual rewrites expose reliable
    transformations.

## Graph IR v1 acceptance gate

Do not call the graph schema stable until two real rewritten workflows execute
end to end on Argo and demonstrate:

- Python type checking and boundary validation;
- a finite dynamic map with bounded concurrency;
- the empty-map path;
- ordered durable reduction;
- exhaustive branching and heterogeneous gather;
- reusable execution contracts and a custom environment;
- JSON/Pydantic, Blob, and Tree crystallization through S3 or MinIO;
- one captured permanent failure;
- one retry followed by process loss and durable resume;
- the same compiled plan producing equivalent canonical outputs locally and on
  Argo.

Tests are functional: real filesystems, object-store-compatible services,
generated plans and manifests, runner processes, and local Kubernetes/Argo where
appropriate. Mock APIs, spies, and patched runtime behavior are not acceptable
evidence for these contracts.

## Deferred decisions

The following should remain open until prototypes or the two rewrites provide
evidence:

- final Python names and decorator syntax;
- exact stable/rerun enum names and any friendly presets;
- the canonical Tree Merkle format;
- cache garbage collection and lease implementation;
- multi-step map-body surface syntax;
- the first non-Argo orchestrator/executor pairing;
- the degree of ergonomic parity in the TypeScript frontend.

The graph IR itself stays intentionally unstable while these questions are
answered. Its version is never omitted, and consumers always compile against an
explicit supported range.

## Rejected directions

- **Metaflow compatibility package:** rejected because applications may be fully
  rewritten for the native Python SDK and preserving implicit state would weaken the new
  model.
- **Pickle compatibility:** rejected in favor of typed, portable artifact kinds.
- **Pydantic Graph as the runtime:** rejected; its authoring form is useful, but
  Massive needs durable target-independent semantics and persistence.
- **A new scheduler inside Massive:** rejected; use target-owned durability.
- **Shared mutable graph state or channels:** rejected in favor of declared
  immutable dataflow and explicit cross-run publication.
- **One effects taxonomy as the primary UX:** rejected for now; expose cache
  substitution and ambiguous-rerun guarantees directly.
- **A large failure-policy IR:** rejected until friendly SDK wrappers prove that
  a new semantic primitive is necessary.
- **Stable Graph IR before real migration:** rejected; keep `0.x`, version every
  artifact, and freeze only after the acceptance gate.
