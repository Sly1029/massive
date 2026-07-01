# Authoring Model

Status: draft

The v0 authoring API is TypeScript-first and functional/declarative. It should feel closer to `pydantic-graph`'s `GraphBuilder` than to a class hierarchy or AST-extracted control flow.

This document describes the intended author-facing model, including features beyond the first portable compiler wedge. The `WorkflowSpec` schema v0 currently admits DAG step nodes, directed edges, and `mergeInputs` fan-in only. Channels, branches, foreach/map, and reducer-backed joins are post-M2 portable-schema work even if the authoring API sketches their eventual shape here.

Authors define:

- a workflow,
- optional globally declared state channels,
- typed steps,
- declarative edges,
- optional branches, foreach/map operations, and joins,
- execution contracts on workflow defaults and step overrides.

Graphology is an internal implementation detail. Authors do not manipulate Graphology directly in the common path, but the SDK uses Graphology for graph construction, validation, analysis, rendering, and IR export.

## Basic Linear Flow

The simplest case is step output flowing into the next step's input:

```ts
const g = workflow({
  name: "math",
  input: z.number(),
  output: z.string(),
  defaults: nodeDefaults,
});

const double = g.step("double", {
  input: z.number(),
  output: z.number(),
  run: async ({ input }) => input * 2,
});

const stringify = g.step("stringify", {
  input: z.number(),
  output: z.string(),
  run: async ({ input }) => `Result: ${input}`,
});

g.start().to(double).to(stringify).to(g.end());
```

Each step return value is persisted as a step output artifact. It is not automatically promoted to a named channel.

## Step Output

A step output is the direct return value of one step. It is local dataflow:

```text
step A returns X
edge A -> B exists
step B receives X as input
```

This is the default authoring model because it is obvious to human readers. A step returns data, and the next step receives that data.

## Channels

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
  run: async ({ input, tools }) => tools.semgrep.scan(input),
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
  run: async ({ input, tools }) => tools.semgrep.scan(input),
});
```

Function projections are local authoring/runtime conveniences. The portable IR should record either serializable projections or named symbols. Arbitrary closures must not be required by backend execution.

## Branches

Branches should default to discriminant-channel switches. This is portable and statically checkable.

```ts
g.branch(scan, {
  on: "risk",
  cases: {
    none: g.path().to(done),
    low: g.path().to(summarize),
    high: g.path().to(triage).to(summarize),
  },
});
```

The compiler validates:

- the channel exists,
- the channel schema is a finite discriminant where possible,
- cases are exhaustive unless an explicit default is provided,
- branch arms remain acyclic.

Arbitrary condition functions should be a future escape hatch represented as named symbols, not inline closures in the IR.

## Foreach And Joins

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
