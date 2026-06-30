# Massive Workflow Compiler Spec

Status: draft

Massive is a portable workflow compiler. It is not, at least initially, a durable execution runtime. Authors define typed workflows in TypeScript, the compiler lowers them into a language-neutral Cap'n Proto workflow plan, and backend compilers render that plan into runnable artifacts such as a local async plan or an Argo deploy bundle.

The core bet is that workflow authoring, graph analysis, execution requirements, environment materialization, and backend-specific deployment can be separated cleanly:

```text
TypeScript authoring source
  -> Graphology-backed graph model
  -> Cap'n Proto WorkflowPlan
  -> object-store datastore
  -> backend compiler / runner
```

All workflows are compiled before running. This includes local development. Local runs may auto-compile by default, but they still run a compiled plan from the local datastore.

## Goals

- Provide a TypeScript-first workflow SDK with a declarative, functional authoring style inspired by `pydantic-graph`.
- Use native graph libraries instead of reimplementing graph algorithms. TypeScript uses Graphology internally; a future Python SDK can use NetworkX while emitting the same IR.
- Keep the canonical workflow representation language-neutral with Cap'n Proto.
- Treat execution requirements as first-class. A compiled workflow includes graph topology plus environment, resources, secrets, storage, network, and observability contracts.
- Support local async execution and Argo as the first production backend.
- Use object storage for compiled plans, code packages, environments, step outputs, channel values, and run artifacts.
- Keep future runtime sidecar/proxy support reserved in the model without requiring it for v0.

## Non-Goals For V0

- Owning a durable execution runtime.
- Supporting arbitrary cyclic workflows. The portable v0 IR is DAG-only.
- Supporting Cloudflare Workers/Workflows or Vercel Workflows as v0 backends.
- Supporting Python authoring in v0.
- Implementing uv, Nix, or container image builders beyond the TypeScript/Node path and container escape hatch.
- Requiring a metadata database or hosted control plane.
- Implementing the full runtime sidecar/proxy security model in v0.

## Core Architecture

Massive has three separate but joined core documents in the compiled plan:

- `GraphIR`: computation topology. Nodes, edges, branches, foreach/map, joins, step symbols, input/output schemas, retry metadata, and artifact edges.
- `ExecutionContract`: how the computation is allowed to run. Environment specs, resources, secrets, network intents, storage requirements, observability, and runtime mediation mode.
- `WorkflowPlan`: the compiled unit that joins `GraphIR`, `ExecutionContract`, symbol tables, materialized artifact references, backend target metadata, and provenance.

Backends consume `WorkflowPlan`. They should not need to inspect TypeScript source.

## V0 Scope

V0 should include:

- TypeScript SDK.
- Graphology-backed builder model.
- Cap'n Proto schema for the canonical IR.
- Local filesystem datastore.
- S3-compatible object-store datastore.
- Local async runner.
- Argo compiler that emits a deploy bundle.
- Node environment materialization from package manager lockfiles.
- Container environment escape hatch.
- Basic field-level provenance map for generated backend artifacts.
- Functional test harnesses for SDK compilation, datastore behavior, environment materialization, and Argo bundle validation. Mocking APIs are banned.

## References

Design was informed by:

- Semgrep Workflows: Python/Metaflow authoring, Argo compilation, S3-backed code packages and artifacts, runtime network/secret isolation work.
- Metaflow: static graph extraction, datastore model, Argo support, and environment plugins such as uv and PyPI.
- Pydantic Graph: builder API with typed state, typed dependencies, typed step input/output, map/spread, join, and reducers.
- Graphology and NetworkX: mature graph libraries that should own graph operations in their respective languages.
- Argo Workflows: the first non-local backend target.
- Cloudflare Workflows, Vercel Workflows, Temporal, Dagster, Inngest, and Mastra: adjacent systems that make the market positioning clearer.
