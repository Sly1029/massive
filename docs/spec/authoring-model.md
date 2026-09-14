# Authoring Model

> **V0 scope:** this document records the TypeScript authoring model. The
> shipped Python surface is documented in
> [`../../packages/python/README.md`](../../packages/python/README.md).
> [Workflow Platform v2 Direction](workflow-platform-v2.md) is normative for the
> Python-first v2 model and supersedes shared-state or channel semantics.
> The unemittable TypeScript channel/state surface has been removed; use explicit
> inputs and outputs.

Status: draft

Both authoring APIs are functional and declarative. The Python `GraphBuilder`
is the primary v2 surface; this document retains the TypeScript forms and the
portable semantics they share.

The current TypeScript builder emits static graphs using Graph IR 0.3. It can
parse shared 0.3 specs and its runner executes scoped map-item descriptors,
but decision and finite-map authoring are currently Python-only surfaces.

This document describes the intended author-facing model, including features
beyond the first portable compiler wedge. `WorkflowSpec` transport schema v0
carries Graph IR 0.3 for static DAGs, exhaustive data-only decisions
and selects, and finite single-step maps with ordered collection. Older Graph IR
artifacts must be rebuilt with the current SDK.
Multi-step map bodies, broadcast/gather, and reducer-backed joins
remain future portable-schema work even where this document sketches their
eventual shape.

Authors define:

- a workflow,
- typed steps,
- declarative edges,
- optional exhaustive decisions, typed selects, and finite maps,
- execution contracts on workflow defaults and step overrides.

Multi-step or streaming maps, broadcasts, gathers, and
reducers are deferred surfaces rather than current portable authoring
features. The current Python finite-map surface is documented in its SDK
README.

Graphology is an internal implementation detail. Authors do not manipulate Graphology directly in the common path, but the SDK uses Graphology for graph construction, validation, analysis, rendering, and IR export.

## Basic Linear Flow

The simplest case is step output flowing into the next step's input:

```ts
const g = workflow({
  name: "math",
  input: z.int(),
  output: z.string(),
  defaults: nodeDefaults,
});

const double = g.step("double", {
  input: z.int(),
  output: z.int(),
  run: async ({ input }) => input * 2,
});

const stringify = g.step("stringify", {
  input: z.int(),
  output: z.string(),
  run: async ({ input }) => `Result: ${input}`,
});

g.start().to(double).to(stringify).to(g.end());
```

Each step return value is persisted as a step output artifact. It is not automatically promoted to a named channel.

V0 artifact values use canonical JSON with safe integers only. In TypeScript,
use `z.int()` for numeric inputs and outputs; `z.number()` is rejected during
emission because it admits fractional values that no portable runtime can
publish canonically.

## Step Output

A step output is the direct return value of one step. It is local dataflow:

```text
step A returns X
edge A -> B exists
step B receives X as input
```

This is the default authoring model because it is obvious to human readers. A step returns data, and the next step receives that data.

## No Shared State or Channel Publication

Persisted step outputs already provide durable dataflow. A second mechanism for
publishing the same values into mutable named channels adds ambiguity without
new execution capability. The former TypeScript `state`, `channel`, and `publish`
fields and their helper exports have been removed.

Pass a typed output along an edge. Use explicit merge inputs for static fan-in,
ordered map collection for dynamic fan-out, and a decision/select for alternatives.
Keep transformations in ordinary named steps instead of serializing closures.

## Exhaustive Decisions

Graph IR 0.3 represents routing as data. An ordinary typed step returns a
Pydantic discriminated union whose variants carry string `Literal` tags. The
author then creates a decision over that persisted output and explicitly wires
every case:

```py
from typing import Annotated, Literal

from pydantic import BaseModel, Field

from massive import StepContext


class Input(BaseModel):
    value: int


class Approved(BaseModel):
    kind: Literal["approved"]
    value: int

class Rejected(BaseModel):
    kind: Literal["rejected"]
    reason: str

Route = Annotated[Approved | Rejected, Field(discriminator="kind")]


class Result(BaseModel):
    value: int


async def classify(context: StepContext[Input]) -> Route:
    if context.inputs.value >= 0:
        return Approved(kind="approved", value=context.inputs.value)
    return Rejected(kind="rejected", reason="negative value")


def approve(context: StepContext[Approved]) -> Result:
    return Result(value=context.inputs.value)


def reject(context: StepContext[Rejected]) -> Result:
    return Result(value=0)

classified = graph.add(classify)
approved = graph.add(approve)
rejected = graph.add(reject)
route = graph.decision(classified, on="kind", id="review-route")

approved_input = route.case(Approved)
rejected_input = route.case(Rejected)
graph.edge_from(approved_input).to(approved)
graph.edge_from(rejected_input).to(rejected)

selected = route.select(Result, approved=approved, rejected=rejected)
graph.edge_from(selected).to(graph.end)
```

The decision IR contains only a selector, string tags, and schema references;
it never serializes a predicate or callable. Complex classification remains an
ordinary named step. There is no default arm: all discriminant variants must
be represented exactly once.

The portable compiler validates that the decision has exactly one value
producer whose output schema equals the decision input schema; conditional
edges exactly cover the declared cases; each conditional target is a step
whose input schema equals that case's schema; and a select covers every case
with branch-local sources whose output schemas equal the select output schema.
The whole graph must remain acyclic.

The local orchestrator executes Graph IR 0.3 decisions and persists each
selected case in the run manifest before scheduling a branch. Argo lowering
supports decision/select nodes with validated route tasks and success-only
branch dependencies. Target capability is therefore explicit rather than implied by
successful portable compilation.

## Deferred Foreach And Joins

Foreach is a dynamic fan-out, not a loop. It is DAG-compatible because it has no back edge.

```ts
const perFile = g.foreach({
  id: "scan-files",
  over: "files",
  body: scanOneFile,
  collect: "findings",
  concurrency: 50,
});

g.path(perFile).to(aggregate).to(g.end());
```

Join behavior is driven by reducers. If multiple upstream paths publish into the same channel, that channel must declare a reducer unless the compiler can prove there is only one writer.

## Fluent Paths

`g.start().to(a).to(b)` is supported as linear sugar. It should return addressable handles internally, not an opaque cursor. Non-linear structures should use explicit operations such as `branch`, `foreach`, `fanout`, and `join`.

This keeps the linear path readable while avoiding a fluent API that becomes confusing at fan-in/fan-out boundaries.

## Execution Contracts In Authoring

Execution contracts belong to a function's use in a graph. Omit `contract=` to
use graph defaults, pass a complete contract to replace them, or use Python's
`dataclasses.replace` to derive a contract while preserving every other field:

```python
from dataclasses import replace

summarize_node = graph.add(
    summarize,
    contract=replace(graph.defaults, cpu="2", memory="4Gi"),
)
```

The TypeScript SDK offers `baseContract.extend(...)` for explicit derivation;
registering an explicit contract replaces graph defaults in both SDKs. Secrets
are logical deployment references. Task code constructs its own service clients;
there is no injected dependency object or shared mutable workflow state.

## Closure Boundary

The TypeScript step function exists for local execution, type inference, and symbol registration. The portable plan must not depend on serializing closures.

Every executable step, reducer, projection, and advanced condition must have a stable symbol identity in the compiled plan. Backend runners resolve symbols through a language/runtime registry.
