# Authoring Model

> **V0 scope:** this document records the TypeScript authoring model. The
> shipped Python surface is documented in
> [`../../packages/python/README.md`](../../packages/python/README.md).
> [Workflow Platform v2 Direction](workflow-platform-v2.md) is normative for the
> Python-first v2 model and supersedes shared-state or channel semantics.

Status: draft

Both authoring APIs are functional and declarative. The Python `GraphBuilder`
is the primary v2 surface; this document retains the TypeScript forms and the
portable semantics they share.

This document describes the intended author-facing model, including features
beyond the first portable compiler wedge. `WorkflowSpec` transport schema v0
carries Graph IR 0.1 static DAGs and Graph IR 0.2 exhaustive, data-only
decisions and selects. Channels, foreach/map, broadcast/gather, and
reducer-backed joins remain future portable-schema work even where this
document sketches their eventual shape.

Authors define:

- a workflow,
- typed steps,
- declarative edges,
- optional exhaustive decisions and typed selects,
- execution contracts on workflow defaults and step overrides.

Named state channels, foreach/map operations, broadcasts, gathers, and
reducers are deferred surfaces rather than current portable authoring
features.

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

## Deferred Channels

A channel is named graph state. It is a typed, durable slot in the workflow plan that steps can read from, publish to, branch on, join into, and expose as final output.

A channel is not a stream in the Kafka sense. It is closer to a named artifact with a schema and merge semantics.

A channel has:

- `name`: stable IR identifier, such as `findings`.
- `schema`: runtime validation schema, usually Zod in the TypeScript SDK.
- `reducer`: how multiple writes combine, such as append, first, last, max, or a named custom reducer.
- optional future visibility/storage hints.

Channels are opt-in. Use them when data must be addressable outside a single edge:

- branch conditions,
- join/foreach collection,
- final output projection,
- cross-branch graph memory,
- debug or durable artifacts that later steps should read by name.

## State Schema

When channels enter the portable schema, they should be globally declared in `stateSchema(...)`.

This decision is tentative and open to change after authoring real workflows. Central declaration is the clearest compile target now because the compiler can validate schemas, reducers, branch discriminants, joins, and final projections up front.

```ts
const State = stateSchema({
  repo: channel(RepoArtifact),
  findings: channel(z.array(Finding), reducers.append),
  risk: channel(z.enum(["none", "low", "high"])),
  summary: channel(Summary),
});
```

## Publishing Step Output To Channels

Step returns are always persisted. Publishing is the separate act of binding a returned value, or part of it, to a named channel.

Common whole-output publish:

```ts
const clone = g.step("clone", {
  input: RepoUrl,
  output: RepoArtifact,
  channel: "repo",
  run: async ({ input, tools }) => tools.git.clone(input),
});
```

Object-field publish:

```ts
const scan = g.step("scan", {
  input: RepoArtifact,
  output: ScanResult,
  publish: {
    findings: "findings",
    risk: "risk",
  },
  run: async ({ input, tools }) => tools.analyzer.scan(input),
});
```

Explicit projections:

```ts
const scan = g.step("scan", {
  input: RepoArtifact,
  output: ScanResult,
  publish: {
    findings: (out) => out.findings,
    risk: (out) => out.risk,
  },
  run: async ({ input, tools }) => tools.analyzer.scan(input),
});
```

Function projections are local authoring/runtime conveniences. The portable IR should record either serializable projections or named symbols. Arbitrary closures must not be required by backend execution.

## Exhaustive Decisions

Graph IR 0.2 represents routing as data. An ordinary typed step returns a
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


@graph.step()
async def classify(context: StepContext[None, Input]) -> Route:
    if context.inputs.value >= 0:
        return Approved(kind="approved", value=context.inputs.value)
    return Rejected(kind="rejected", reason="negative value")


@graph.step()
def approve(context: StepContext[None, Approved]) -> Result:
    return Result(value=context.inputs.value)


@graph.step()
def reject(context: StepContext[None, Rejected]) -> Result:
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

The local orchestrator executes Graph IR 0.2 decisions and persists each
selected case in the run manifest before scheduling a branch. Argo lowering
currently rejects decision/select nodes with an explicit unsupported-semantic
diagnostic. Target capability is therefore explicit rather than implied by
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

Execution contracts are declared inline by default, with reusable fragments for common cases.

```ts
const baseContract = contract({
  env: env.node({
    version: "22.12.0",
    packageManager: "pnpm",
    lockfile: "pnpm-lock.yaml",
  }),
  resources: { cpu: "0.5", memory: "512Mi" },
  network: net.denyAll(),
});

const callOpenAI = baseContract.extend({
  secrets: [secret.ref("OPENAI_API_KEY")],
  network: net.allow("api.openai.com"),
});

const summarize = g.step("summarize", {
  input: ScanResult,
  output: Summary,
  contract: callOpenAI,
  run: async ({ input, deps }) => deps.openAI.summarize(input.findings),
});
```

Workflows may define defaults. Step contracts override defaults at compile time.

```ts
const g = workflow({
  name: "repo-triage",
  state: State,
  output: Summary,
  defaults: baseContract,
});
```

## Closure Boundary

The TypeScript step function exists for local execution, type inference, and symbol registration. The portable plan must not depend on serializing closures.

Every executable step, reducer, projection, and advanced condition must have a stable symbol identity in the compiled plan. Backend runners resolve symbols through a language/runtime registry.
