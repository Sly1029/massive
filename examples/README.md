# Graph walkthrough

These examples build one idea at a time. Start at `01` if you are new to
Massive; each following file keeps the same static, typed graph model and adds
one feature.

Run commands from the repository root. Install the checked-in dependencies
first if needed:

```sh
pnpm install --frozen-lockfile
```

Examples 01–04 exercise the existing TypeScript frontend through the legacy Deno
CLI. Python examples use the shipped `massive` CLI from a platform wheel. For a
source checkout, build/install the wheel first; an editable Python SDK alone does
not include the native CLI.

## 1. The smallest graph: passthrough

[`01-passthrough.ts`](01-passthrough.ts) has only Massive's two sentinel nodes:

```text
__start -> __end
```

`workflow()` declares the workflow's name and its boundary schemas. Zod schemas
are both TypeScript type sources and runtime validation boundaries. Because no
step transforms the value, input and output schemas are identical.

Run it locally:

```sh
deno task massive run examples/01-passthrough.ts \
  --input '{"message":"hello"}'
```

The graph is static: construct its nodes and edges when the module loads. Step
results can choose data and routes, but they do not create new graph structure.

## 2. Add typed steps

[`02-linear.ts`](02-linear.ts) introduces two steps:

```text
__start -> double -> label -> __end
```

Each `graph.step(id, { input, output, run })` has an input schema, an output
schema, and executable code. The path builder tracks the current output type,
so `.to(next)` only accepts a step whose input matches it.

The step functions are top-level named exports. The emitted plan stores symbol
references, not function bodies, and the runner resolves those exports in a
fresh step process.

```sh
deno task massive run examples/02-linear.ts --input '{"value":21}'
```

The result is `{"message":"value:42"}`.

## 3. Fan out, then fan in

[`03-diamond.ts`](03-diamond.ts) is the first non-linear graph:

```text
                 -> addOne -
                /           \
__start -> split              -> total -> __end
                \           /
                 -> triple --
```

`graph.from(root)` starts another path at an existing node. Both downstream
steps receive `split`'s output and may run independently. `graph.merge([...])`
joins them; its target receives an array in the same order as the handles in
the merge call.

```sh
deno task massive run examples/03-diamond.ts --input '20'
```

`addOne` returns `21`, `triple` returns `60`, and `total` returns `81`.

Use `merge` only for an explicit fan-in. A normal step has one upstream value;
a merge target declares `z.array(...)` and receives all listed upstream values.

## 4. Declare execution requirements

Graph shape says *what depends on what*. Execution contracts say *what a step
needs to run*. [`04-contracts.ts`](04-contracts.ts) adds workflow defaults for
the runtime, resources, and network, then extends them for one step:

- the step inherits `cpu: 0.5` and overrides memory from `512Mi` to `1Gi`;
- it declares one secret by logical name and backing reference;
- it replaces deny-all egress with one allowed host.

Contracts are plan data. They do not read secrets or make network calls while
the graph is being authored. Emit this example to inspect the resolved,
deduplicated environment and contract entries:

```sh
deno run --allow-read --allow-sys=cpus examples/emit.ts \
  examples/04-contracts.ts > /tmp/contracts-spec.json
```

Container environments use immutable `image@sha256:...` references. The node
environment here keeps the example runnable from this repository checkout.

## 5. Route over an exhaustive decision

Decisions are currently exposed by the Python frontend.
[`05-decision/workflow.py`](05-decision/workflow.py) models the classifier
output as a Pydantic discriminated union:

```text
                       approved -> approve -
                      /                      \
__start -> classify -> route                  -> select -> __end
                      \                      /
                       rejected -> reject ---
```

`route.case(Model)` narrows the branch input type. `route.select(...)` joins
the alternatives back into one result handle. Every discriminator case must be
claimed, and only the selected branch runs; inactive branch steps are skipped.

```sh
massive run examples/05-decision/workflow.py --project examples/decision --input '{"score":82}'
massive run examples/05-decision/workflow.py --project examples/decision --input '{"score":40}'
```

Decision/select graphs run on the local target. Argo lowering currently rejects
them rather than silently changing their semantics.

## 6. Map a finite collection

[`06-map/workflow.py`](06-map/workflow.py) turns a concrete list into bounded
parallel work:

```text
__start -> unpack -> map square (concurrency 4) -> __end
```

`graph.map(source, mapper, concurrency=4)` creates one Graph IR map node. The
mapper receives one item per invocation; the resulting list preserves source
order even if items finish out of order. Empty input produces an empty list.

```sh
massive run examples/06-map/workflow.py --project examples/map \
  --input '{"values":[2,3,4,5]}'
```

Finite maps run locally and lower to bounded native Argo fan-out. Maps use Graph
IR 0.3; decisions use 0.2; ordinary TypeScript step graphs use 0.1.

## 7. Package modules and resources, then collect map results

[`07-package/workflow.py`](07-package/workflow.py) imports its mapper from a
workflow-local package. The mapper reads a text resource using
`importlib.resources`; `[tool.massive.source].include` in the adjacent
[`pyproject.toml`](07-package/pyproject.toml) selects both code and resource bytes.

```sh
massive run examples/07-package/workflow.py --project examples/package \
  --input '{"values":[3,1,3]}'
```

The result is `{"labels":["item:3","item:1","item:3"]}`. Duplicate inputs remain
separate items and output ordering follows source order. The map's collected
`list[str]` feeds an ordinary typed step, which constructs the final result.
The clean-wheel distribution test also executes this mapper from the generated
bundle after moving the original checkout.

### What we take from Pydantic Graph joins

[Pydantic Graph's joins](https://ai.pydantic.dev/graph/beta/joins/) synchronize
the work belonging to a parent fork, reduce incoming values, and finalize once
that fork completes. That scope-aware model is useful inspiration for future
multi-step map bodies; a join is not simply every predecessor in the entire graph.

Massive currently crystallizes a finite list, maps each source index, and collects
an ordered result list before invoking an ordinary aggregation step. This keeps
empty input, duplicates, failure, and ordering explicit without a second reducer
runtime. A decision's `select` chooses one alternative; it is not a parallel
reducer join. Mutable shared state and completion-order reduction are not copied
from the in-memory model.

## From graph code to protobuf

There are two durable boundaries:

```text
authoring module -> WorkflowSpec JSON -> WorkflowPlan protobuf -> target bundle
                    frontend output      compiler output
```

The frontend lowers Zod or Pydantic schemas, graph nodes and edges, executable
symbol references, source-package hashes, environments, and contracts into a
portable `WorkflowSpec`. The Go compiler validates that spec and constructs a
typed `WorkflowPlan` defined in
[`conformance/schema/workflow-plan.proto`](../conformance/schema/workflow-plan.proto).

The graph-related protobuf messages are, in abbreviated form:

```proto
message WorkflowPlan {
  optional uint32 schema_version = 1;
  optional string plan_hash = 2;
  optional string spec_hash = 3;
  optional GraphIR graph = 4;
  repeated SchemaEntry schemas = 5;
  repeated SymbolEntry symbols = 6;
  repeated SourcePackageRef source_packages = 7;
  repeated MaterializedEnvironment environments = 8;
  repeated ExecutionContract contracts = 9;
}

message GraphIR {
  optional string workflow_name = 1;
  optional string input_schema = 2;
  optional string output_schema = 3;
  optional string start_node = 4;
  optional string end_node = 5;
  repeated GraphNode nodes = 6;
  repeated GraphEdge edges = 7;
  optional string ir_version = 8;
}

message GraphNode {
  optional string id = 1;
  optional string kind = 2;
  optional string input_schema = 3;
  optional string output_schema = 4;
  optional string symbol_ref = 5;
  optional string contract_ref = 6;
  repeated string merge_inputs = 7;
  // Later fields carry decision, select, and map metadata.
}

message GraphEdge {
  optional string from = 1;
  optional string to = 2;
  optional string case = 3; // present only on decision edges
}
```

The committed plan artifact is protobuf's canonical JSON projection rather than
binary protobuf. Fields therefore use lower camel case. For the diamond, the
interesting portion looks like this (hash references are shortened here):

```json
{
  "graph": {
    "workflowName": "diamond-example",
    "startNode": "__start",
    "endNode": "__end",
    "nodes": [
      { "id": "__start", "kind": "start" },
      {
        "id": "total",
        "kind": "step",
        "inputSchema": "sha256:...",
        "outputSchema": "sha256:...",
        "symbolRef": "ts-main:03-diamond.ts#total",
        "contractRef": "sha256:...",
        "mergeInputs": ["addOne", "triple"]
      },
      { "id": "__end", "kind": "end" }
    ],
    "edges": [
      { "from": "__start", "to": "split" },
      { "from": "addOne", "to": "total" },
      { "from": "split", "to": "addOne" },
      { "from": "split", "to": "triple" },
      { "from": "total", "to": "__end" },
      { "from": "triple", "to": "total" }
    ],
    "irVersion": "0.1"
  }
}
```

Node and edge arrays are canonicalized, so declaration order does not define
artifact identity or execution order. Edges express dependencies; the compiler
derives a valid schedule from them.

Generate the full spec and plan for the diamond:

```sh
deno run --allow-read --allow-sys=cpus examples/emit.ts \
  examples/03-diamond.ts > /tmp/diamond-spec.json

go run ./cmd/massive-compiler compile \
  --spec /tmp/diamond-spec.json \
  --out /tmp/diamond-plan

uv run --project packages/python --frozen python -m json.tool \
  /tmp/diamond-plan/workflow-plan.json
```

The full schema also describes hashing recipes, source packages, materialized
environments, target plans, and compiler provenance. See
[`workflow-plan-json-projection.md`](../conformance/schema/workflow-plan-json-projection.md)
for the exact JSON projection and compatibility rules.

## Useful rules of thumb

- Give every step a stable, unique ID and a top-level exported implementation.
- Treat schemas as durable API boundaries, not just editor hints.
- Use edges for dependencies, `merge` for fan-in, decisions for exhaustive
  routing, and maps only for finite lists.
- Keep graph construction free of runtime-dependent control flow.
- Declare resources, secrets, and network access in contracts so targets can
  enforce them.
- Inspect the emitted spec or compiled plan when debugging topology; it is the
  portable truth consumed by every backend.
